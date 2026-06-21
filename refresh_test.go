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

	expectedAuthByAccount map[string]string
	responseByAccount     map[string]pluginapi.HTTPResponse
	httpStatus            int
	doStarted             chan struct{}
	releaseDo             chan struct{}

	mu           sync.Mutex
	httpCalls    int
	activeHTTP   int
	maxActive    int
	headerErrors []string
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
	if f.doStarted != nil {
		select {
		case f.doStarted <- struct{}{}:
		default:
		}
	}
	if f.releaseDo != nil {
		<-f.releaseDo
	}
	defer func() {
		f.mu.Lock()
		f.activeHTTP--
		f.mu.Unlock()
	}()

	accountID := req.Headers.Get("Chatgpt-Account-Id")
	if accountID == "" {
		f.recordHeaderError("missing Chatgpt-Account-Id header")
	} else if accountID == "acct-1" && f.expectedAuthByAccount == nil && req.Headers.Get("Authorization") != "Bearer access-1" {
		f.recordHeaderError("Authorization header = " + req.Headers.Get("Authorization"))
	}
	if expected := f.expectedAuthByAccount[accountID]; expected != "" && req.Headers.Get("Authorization") != expected {
		f.recordHeaderError("Authorization header = " + req.Headers.Get("Authorization") + ", want " + expected)
	}
	if req.Headers.Get("Content-Type") != "application/json" {
		f.recordHeaderError("Content-Type header = " + req.Headers.Get("Content-Type"))
	}
	if req.Headers.Get("User-Agent") != quotaUserAgent {
		f.recordHeaderError("User-Agent header = " + req.Headers.Get("User-Agent"))
	}

	if resp, ok := f.responseByAccount[accountID]; ok {
		return resp, nil
	}
	status := f.httpStatus
	if status == 0 {
		status = http.StatusOK
	}
	return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{}, Body: f.httpBody}, nil
}

func (f *fakeHostClient) Log(level, message string, fields map[string]any) {}

func (f *fakeHostClient) recordHeaderError(message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headerErrors = append(f.headerErrors, message)
}

func (f *fakeHostClient) assertNoHeaderErrors(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.headerErrors) > 0 {
		t.Fatalf("host request header errors: %v", f.headerErrors)
	}
}

func (f *fakeHostClient) maxActiveHTTP() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func (f *fakeHostClient) httpCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.httpCalls
}

func (f *fakeHostClient) activeHTTPCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activeHTTP
}

func accountByAuthID(t *testing.T, snapshot StateSnapshot, authID string) AccountState {
	t.Helper()
	for _, account := range snapshot.Accounts {
		if account.AuthID == authID {
			return account
		}
	}
	t.Fatalf("missing account with auth ID %q in %#v", authID, snapshot.Accounts)
	return AccountState{}
}

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
	host.assertNoHeaderErrors(t)
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	if snapshot.Accounts[0].ChatGPTAccountID != "acct-1" {
		t.Fatalf("account = %#v", snapshot.Accounts[0])
	}
	if snapshot.Accounts[0].LastError != "" {
		t.Fatalf("LastError = %q, want empty", snapshot.Accounts[0].LastError)
	}
	if snapshot.Accounts[0].LastSuccessAt.IsZero() {
		t.Fatalf("LastSuccessAt is zero, want successful refresh")
	}
	if snapshot.Accounts[0].Quota.FiveHour == nil {
		t.Fatalf("FiveHour quota is nil, want parsed quota")
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

func TestRefreshOnceHTTPNon2xxRecordsRedactedErrorWithoutQuotaSuccess(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpStatus: http.StatusUnauthorized,
		httpBody:   []byte(`upstream rejected access-1`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	host.assertNoHeaderErrors(t)

	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.LastError == "" {
		t.Fatalf("LastError empty, want HTTP status error")
	}
	if strings.Contains(account.LastError, "access-1") {
		t.Fatalf("LastError leaked token: %q", account.LastError)
	}
	if !account.LastSuccessAt.IsZero() {
		t.Fatalf("LastSuccessAt = %v, want zero", account.LastSuccessAt)
	}
	if account.Quota.FiveHour != nil {
		t.Fatalf("FiveHour quota = %#v, want nil", account.Quota.FiveHour)
	}
}

func TestRefreshOnceContinuesAfterOneAccountFails(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken2 := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-2"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "auth-fail", AuthIndex: "idx-fail", Provider: "codex"},
			{ID: "auth-ok", AuthIndex: "idx-ok", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-fail": json.RawMessage(`{"access_token":"secret-access-fail","id_token":"bad-token"}`),
			"idx-ok":   json.RawMessage(`{"access_token":"access-2","id_token":"` + idToken2 + `"}`),
		},
		expectedAuthByAccount: map[string]string{
			"acct-2": "Bearer access-2",
		},
		responseByAccount: map[string]pluginapi.HTTPResponse{
			"acct-2": {
				StatusCode: http.StatusOK,
				Headers:    http.Header{},
				Body:       []byte(`{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
			},
		},
	}
	store := NewPluginState(DefaultConfig())
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	host.assertNoHeaderErrors(t)

	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(snapshot.Accounts))
	}
	failed := accountByAuthID(t, snapshot, "auth-fail")
	if failed.LastError == "" {
		t.Fatalf("failed account LastError empty")
	}
	if strings.Contains(failed.LastError, "secret-access-fail") {
		t.Fatalf("failed account LastError leaked token: %q", failed.LastError)
	}
	success := accountByAuthID(t, snapshot, "auth-ok")
	if success.LastError != "" {
		t.Fatalf("success account LastError = %q, want empty", success.LastError)
	}
	if success.LastSuccessAt.IsZero() || success.Quota.FiveHour == nil {
		t.Fatalf("success account was not refreshed: %#v", success)
	}
}

func TestRefreshSoonDoesNotOverlapRefreshes(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody:   []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		doStarted:  make(chan struct{}, 1),
		releaseDo:  make(chan struct{}),
		httpStatus: http.StatusOK,
	}
	store := NewPluginState(DefaultConfig())
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })

	refresher.RefreshSoon()
	select {
	case <-host.doStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first refresh to start")
	}
	for i := 0; i < 10; i++ {
		refresher.RefreshSoon()
	}
	if !waitUntil(time.Second, func() bool { return host.httpCallCount() == 1 }) {
		t.Fatalf("http calls = %d, want 1 while refresh is active", host.httpCallCount())
	}
	host.releaseDo <- struct{}{}
	if !waitUntil(time.Second, func() bool { return host.activeHTTPCount() == 0 }) {
		t.Fatalf("active HTTP = %d, want 0 after releasing request", host.activeHTTPCount())
	}
	if host.maxActiveHTTP() != 1 {
		t.Fatalf("max active HTTP = %d, want 1", host.maxActiveHTTP())
	}
	host.assertNoHeaderErrors(t)
}

func waitUntil(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return condition()
}
