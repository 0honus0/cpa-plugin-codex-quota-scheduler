package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type coreTestHost struct {
	mu       sync.Mutex
	auths    []pluginapi.HostAuthFileEntry
	authJSON map[string]json.RawMessage
	quota    pluginapi.HTTPResponse
	requests []pluginapi.HTTPRequest
	disabled map[string]bool
}

func (h *coreTestHost) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pluginapi.HostAuthFileEntry(nil), h.auths...), nil
}

func (h *coreTestHost) GetAuth(index string) (pluginapi.HostAuthGetResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return pluginapi.HostAuthGetResponse{AuthIndex: index, Name: index + ".json", JSON: append(json.RawMessage(nil), h.authJSON[index]...)}, nil
}

func (h *coreTestHost) SetAuthDisabled(authIndex string, disabled bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.disabled == nil {
		h.disabled = make(map[string]bool)
	}
	h.disabled[authIndex] = disabled
	for i := range h.auths {
		if h.auths[i].AuthIndex == authIndex {
			h.auths[i].Disabled = disabled
		}
	}
	return nil
}

func (h *coreTestHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	resp := h.quota
	h.mu.Unlock()
	if resp.StatusCode == 0 {
		resp.StatusCode = http.StatusOK
	}
	return resp, nil
}

func (h *coreTestHost) Log(string, string, map[string]any) {}

func coreTestQuota(now time.Time, used5h, usedLong float64) ParsedQuota {
	fiveReset := now.Add(4 * time.Hour)
	longReset := now.Add(4 * 24 * time.Hour)
	return ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used5h, ResetAt: fiveReset},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &usedLong, ResetAt: longReset},
	}
}

func coreTestQuotaJSON() []byte {
	return []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":14400},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":345600}}}`)
}

func newCoreTestEngine(t *testing.T, host HostClient, now time.Time) *CoreEngine {
	t.Helper()
	return NewCoreEngine(host, filepath.Join(t.TempDir(), "core-v2.json"), func() time.Time { return now })
}

func TestCoreRosterIncludesAllEnabledPrioritiesAndRefreshDefaultsOn(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	host := &coreTestHost{auths: []pluginapi.HostAuthFileEntry{
		{ID: "high", AuthIndex: "idx-high", Provider: "codex", Priority: 9},
		{ID: "low", AuthIndex: "idx-low", Provider: "codex", Priority: 8},
		{ID: "off", AuthIndex: "idx-off", Provider: "codex", Priority: 7, Disabled: true},
	}}
	engine := newCoreTestEngine(t, host, now)
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	accounts := engine.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("accounts=%d, want 2 enabled Codex accounts", len(accounts))
	}
	if accounts[0].Priority != 9 || accounts[1].Priority != 8 {
		t.Fatalf("priorities=%d,%d, want 9,8", accounts[0].Priority, accounts[1].Priority)
	}
	for _, account := range accounts {
		if !account.RefreshEnabled() {
			t.Fatalf("%s refresh default=false, want true", account.ID)
		}
	}
}

func TestCoreSchedulerNeverCrossesCPAPriorityAndOnlyOrdersWithinTier(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	host := &coreTestHost{auths: []pluginapi.HostAuthFileEntry{
		{ID: "high", AuthIndex: "idx-high", Provider: "codex", Priority: 9},
		{ID: "low", AuthIndex: "idx-low", Provider: "codex", Priority: 8},
	}}
	engine := newCoreTestEngine(t, host, now)
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	engine.finishRefreshSuccess("high", coreTestQuota(now, 95, 20))
	engine.finishRefreshSuccess("low", coreTestQuota(now, 5, 5))
	decision := engine.Pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "low", Provider: "codex", Priority: 8},
		{ID: "high", Provider: "codex", Priority: 9},
	}})
	if decision.AuthID != "high" {
		t.Fatalf("picked %q, want high CPA priority account", decision.AuthID)
	}

	// Once CPA presents accounts in the same priority tier, plugin priority is allowed.
	ten := 10
	if err := engine.SetAccountPreference("low", nil, &ten, nil); err != nil {
		t.Fatal(err)
	}
	decision = engine.Pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Provider: "codex", Priority: 9},
		{ID: "low", Provider: "codex", Priority: 9},
	}})
	if decision.AuthID != "low" {
		t.Fatalf("same-tier pick=%q, want low due scheduler_priority", decision.AuthID)
	}
}

func TestCoreUsage429AutoBanUsesRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	host := &coreTestHost{auths: []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9}}}
	engine := newCoreTestEngine(t, host, now)
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	engine.HandleUsage(pluginapi.UsageRecord{Provider: "codex", AuthID: "a", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429}, ResponseHeaders: http.Header{"Retry-After": []string{"120"}}})
	account, _ := engine.accountByID("a")
	want := now.Add(120 * time.Second)
	if !account.BanUntil.Equal(want) || !account.Banned(now) {
		t.Fatalf("ban_until=%v, want %v", account.BanUntil, want)
	}
}

func TestCoreUsage401DisablesUntilCredentialChanges(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	host := &coreTestHost{
		auths:    []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9}},
		authJSON: map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access-a","refresh_token":"refresh-a","account_id":"acct-a"}`)},
		quota:    pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: coreTestQuotaJSON()},
	}
	engine := newCoreTestEngine(t, host, now)
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	if err := engine.RefreshAccount("a"); err != nil {
		t.Fatal(err)
	}
	engine.HandleUsage(pluginapi.UsageRecord{Provider: "codex", AuthID: "a", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 401}})
	account, _ := engine.accountByID("a")
	if !account.Disabled401 {
		t.Fatal("account not disabled after business 401")
	}
	host.mu.Lock()
	disabledInCPA := host.disabled["idx-a"]
	host.mu.Unlock()
	if !disabledInCPA {
		t.Fatal("CPA auth disabled flag was not set after business 401")
	}
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	if _, ok := engine.accountByID("a"); !ok {
		t.Fatal("401-managed disabled account disappeared from scheduler roster")
	}
	host.mu.Lock()
	host.authJSON["idx-a"] = json.RawMessage(`{"access_token":"access-b","refresh_token":"refresh-b","account_id":"acct-a"}`)
	host.mu.Unlock()
	engine.recheckCredentialOnly(account)
	account, _ = engine.accountByID("a")
	if account.Disabled401 {
		t.Fatal("account remained disabled after credential fingerprint changed")
	}
	host.mu.Lock()
	disabledInCPA = host.disabled["idx-a"]
	host.mu.Unlock()
	if disabledInCPA {
		t.Fatal("CPA auth remained disabled after credential fingerprint changed")
	}
}

func TestCoreFiveHourLazyResetProbeDefaultsToFiveMinutesAfterReset(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	engine := newCoreTestEngine(t, &coreTestHost{}, now)
	oldReset := now.Add(-time.Minute)
	used := 100.0
	previous := ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: oldReset, Exhausted: true}}
	current := cloneCoreQuota(previous)
	account := &CoreAccount{ID: "a"}
	engine.mu.Lock()
	engine.updateProbeScheduleLocked(account, previous, current, now)
	engine.mu.Unlock()
	want := oldReset.Add(5 * time.Minute)
	if !account.ProbeDueAt.Equal(want) {
		t.Fatalf("probe_due=%v, want %v", account.ProbeDueAt, want)
	}
}

func TestCoreProbeRequestPreservesExistingRequestContentByteForByte(t *testing.T) {
	credentials := CodexCredentials{AccessToken: "access", ChatGPTAccountID: "acct"}
	req := coreProbeRequest(credentials)
	if req.URL != codexResetProbeEndpoint {
		t.Fatalf("probe URL=%q, want %q", req.URL, codexResetProbeEndpoint)
	}
	if !bytes.Equal(req.Body, resetProbePayloadBytes()) {
		t.Fatalf("probe body changed: got %q want %q", req.Body, resetProbePayloadBytes())
	}
}

func TestCorePersistentStateNeverContainsRawCredentials(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "core-v2.json")
	host := &coreTestHost{
		auths:    []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9}},
		authJSON: map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access-secret","refresh_token":"refresh-secret","account_id":"acct-a"}`)},
		quota:    pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: coreTestQuotaJSON()},
	}
	engine := NewCoreEngine(host, path, func() time.Time { return now })
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	if err := engine.RefreshAccount("a"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("access-secret"), []byte("refresh-secret"), []byte("access_token"), []byte("refresh_token")} {
		if bytes.Contains(raw, secret) {
			t.Fatalf("persistent state leaked credential material %q", secret)
		}
	}
}

func TestCoreRefreshPreferenceCanDisableOneAccountWithoutChangingCPAPriority(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	host := &coreTestHost{auths: []pluginapi.HostAuthFileEntry{
		{ID: "high", AuthIndex: "idx-high", Provider: "codex", Priority: 9},
		{ID: "low", AuthIndex: "idx-low", Provider: "codex", Priority: 8},
	}}
	engine := newCoreTestEngine(t, host, now)
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	off := false
	if err := engine.SetAccountPreference("low", &off, nil, nil); err != nil {
		t.Fatal(err)
	}
	accounts := engine.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("accounts=%d, want 2", len(accounts))
	}
	for _, account := range accounts {
		if account.ID == "low" {
			if account.RefreshEnabled() {
				t.Fatal("low refresh remained enabled")
			}
			if account.Priority != 8 {
				t.Fatalf("low CPA priority=%d, want 8", account.Priority)
			}
		}
	}
}

func TestCore429BanOnlyAffectsSameTierSelection(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	host := &coreTestHost{auths: []pluginapi.HostAuthFileEntry{
		{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9},
		{ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: 9},
	}}
	engine := newCoreTestEngine(t, host, now)
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	engine.finishRefreshSuccess("a", coreTestQuota(now, 5, 5))
	engine.finishRefreshSuccess("b", coreTestQuota(now, 10, 10))
	engine.HandleUsage(pluginapi.UsageRecord{Provider: "codex", AuthID: "a", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429}, ResponseHeaders: http.Header{"Retry-After": []string{"120"}}})
	decision := engine.Pick(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "a", Provider: "codex", Priority: 9},
		{ID: "b", Provider: "codex", Priority: 9},
	}})
	if decision.AuthID != "b" {
		t.Fatalf("picked %q, want b while a is 429-banned", decision.AuthID)
	}
}
