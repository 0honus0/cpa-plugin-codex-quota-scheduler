package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type fakeHostClient struct {
	authList []pluginapi.HostAuthFileEntry
	authJSON map[string]json.RawMessage
	httpBody []byte

	mu         sync.Mutex
	httpCalls  int
	activeHTTP int
	maxActive  int
}

func (f *fakeHostClient) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	return f.authList, nil
}

func (f *fakeHostClient) GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	return pluginapi.HostAuthGetResponse{AuthIndex: authIndex, JSON: f.authJSON[authIndex]}, nil
}

func (f *fakeHostClient) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.httpCalls++
	f.activeHTTP++
	if f.activeHTTP > f.maxActive {
		f.maxActive = f.activeHTTP
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.activeHTTP--
		f.mu.Unlock()
	}()

	if req.Headers.Get("Authorization") != "Bearer access-1" {
		return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte("bad auth")}, nil
	}
	if req.Headers.Get("Chatgpt-Account-Id") != "acct-1" {
		return pluginapi.HTTPResponse{StatusCode: 400, Body: []byte("bad account")}, nil
	}
	return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{}, Body: f.httpBody}, nil
}

func (f *fakeHostClient) Log(level, message string, fields map[string]any) {}

func TestRefreshOnceLoadsCodexAuthAndQuota(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex", Email: "a@example.com", Priority: 7,
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	if snapshot.Accounts[0].ChatGPTAccountID != "acct-1" {
		t.Fatalf("account = %#v", snapshot.Accounts[0])
	}
}

func TestRefreshOnceSkipsIneligibleAuthEntries(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "disabled", AuthIndex: "idx-disabled", Provider: "codex", Disabled: true},
			{ID: "unavailable", AuthIndex: "idx-unavailable", Provider: "codex", Unavailable: true},
			{ID: "other", AuthIndex: "idx-other", Provider: "openai"},
			{ID: "missing-index", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{},
	}
	store := NewPluginState(DefaultConfig())
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	if got := host.httpCalls; got != 0 {
		t.Fatalf("http calls = %d, want 0", got)
	}
}

func TestRefreshOnceRecordsRedactedErrorWithoutTokenLeak(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"secret-access-1","id_token":"bad-token"}`),
		},
	}
	store := NewPluginState(DefaultConfig())
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	if snapshot.Accounts[0].LastError == "" {
		t.Fatalf("LastError empty, want redacted error")
	}
	if strings.Contains(snapshot.Accounts[0].LastError, "secret-access-1") {
		t.Fatalf("LastError leaked token: %q", snapshot.Accounts[0].LastError)
	}
}
