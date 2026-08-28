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
	return NewCoreEngine(host, filepath.Join(t.TempDir(), "cron.json"), func() time.Time { return now })
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
	engine.HandleUsage(pluginapi.UsageRecord{Provider: "codex", AuthID: "a", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429}, ResponseHeaders: http.Header{"Retry-After": []string{"10"}}})
	account, _ = engine.accountByID("a")
	if !account.BanUntil.Equal(want) {
		t.Fatalf("later shorter 429 shortened ban: got %v want %v", account.BanUntil, want)
	}
	engine.HandleUsage(pluginapi.UsageRecord{Provider: "codex", AuthID: "a"})
	account, _ = engine.accountByID("a")
	if !account.BanUntil.IsZero() || account.BanReason != "" {
		t.Fatalf("successful business request did not clear usage_429 ban: until=%v reason=%q", account.BanUntil, account.BanReason)
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
	engine.RequestRefreshOne("a")
	engine.RunCycle()
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

func TestCoreFutureFiveHourResetIsScheduledImmediately(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	engine := newCoreTestEngine(t, &coreTestHost{}, now)
	resetAt := now.Add(20 * time.Minute)
	used := 73.0
	current := ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &used, ResetAt: resetAt}}
	account := &CoreAccount{ID: "a"}
	engine.mu.Lock()
	engine.updateProbeScheduleLocked(account, ParsedQuota{}, current, now)
	engine.mu.Unlock()
	want := resetAt.Add(5 * time.Minute)
	if !account.ProbeDueAt.Equal(want) {
		t.Fatalf("probe_due=%v, want %v", account.ProbeDueAt, want)
	}
	if account.ProbeStatus != "scheduled_after_reset" {
		t.Fatalf("probe_status=%q, want scheduled_after_reset", account.ProbeStatus)
	}
}

func TestCoreBusiness429BanDoesNotBlockQuotaMaintenance(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 5, 0, 0, time.UTC)
	host := &coreTestHost{
		auths: []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9}},
		authJSON: map[string]json.RawMessage{
			"idx-a": json.RawMessage(`{"access_token":"access","account_id":"acct-a"}`),
		},
		quota: pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: coreTestQuotaJSON()},
	}
	engine := newCoreTestEngine(t, host, now)
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	engine.accounts["a"].LastRefreshAt = now.Add(-31 * time.Minute)
	engine.accounts["a"].BanUntil = now.Add(time.Hour)
	engine.accounts["a"].BanReason = "usage_429"
	engine.mu.Unlock()

	engine.RunCycle()
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) == 0 {
		t.Fatal("429 business ban suppressed quota maintenance")
	}
	if host.requests[0].Method != http.MethodGet || host.requests[0].URL != coreQuotaEndpoint {
		t.Fatalf("maintenance request=%s %s, want GET %s", host.requests[0].Method, host.requests[0].URL, coreQuotaEndpoint)
	}
}

func TestCoreRosterRehydratesProbeScheduleFromPersistedQuota(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 50, 0, 0, time.UTC)
	host := &coreTestHost{auths: []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9}}}
	engine := newCoreTestEngine(t, host, now)
	quota := coreTestQuota(now, 10, 20)
	engine.mu.Lock()
	engine.persisted["a"] = corePersistedAccount{Quota: quota}
	engine.mu.Unlock()

	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	account, ok := engine.accountByID("a")
	if !ok {
		t.Fatal("account not restored")
	}
	want := quota.FiveHour.ResetAt.Add(5 * time.Minute)
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
	path := filepath.Join(t.TempDir(), "cron.json")
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

func TestCoreRegisterPreservesSavedSettingsWhenPluginYAMLIsUnchanged(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cron.json")
	raw := []byte("quota_refresh_interval: 30m\nreset_probe_after_reset_delay: 5m\n")
	engine := NewCoreEngine(&coreTestHost{}, path, func() time.Time { return now })
	if err := engine.ConfigureOnRegister(raw); err != nil {
		t.Fatal(err)
	}
	cfg := engine.Config()
	cfg.ResetProbeAfterResetDelay = 11 * time.Minute
	if err := engine.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}

	restarted := NewCoreEngine(&coreTestHost{}, path, func() time.Time { return now })
	if err := restarted.ConfigureOnRegister(raw); err != nil {
		t.Fatal(err)
	}
	if got := restarted.Config().ResetProbeAfterResetDelay; got != 11*time.Minute {
		t.Fatalf("saved UI setting lost on unchanged YAML restart: got %v want 11m", got)
	}
}

func TestCoreRegisterAppliesChangedPluginYAML(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cron.json")
	oldRaw := []byte("quota_refresh_interval: 30m\nreset_probe_after_reset_delay: 5m\n")
	engine := NewCoreEngine(&coreTestHost{}, path, func() time.Time { return now })
	if err := engine.ConfigureOnRegister(oldRaw); err != nil {
		t.Fatal(err)
	}
	cfg := engine.Config()
	cfg.ResetProbeAfterResetDelay = 11 * time.Minute
	if err := engine.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}

	newRaw := []byte("quota_refresh_interval: 30m\nreset_probe_after_reset_delay: 7m\n")
	restarted := NewCoreEngine(&coreTestHost{}, path, func() time.Time { return now })
	if err := restarted.ConfigureOnRegister(newRaw); err != nil {
		t.Fatal(err)
	}
	if got := restarted.Config().ResetProbeAfterResetDelay; got != 7*time.Minute {
		t.Fatalf("changed plugin YAML was not applied: got %v want 7m", got)
	}
}

func TestCoreProbeSettingsRescheduleCanonicalDueAndClearWhenDisabled(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	engine := newCoreTestEngine(t, &coreTestHost{}, now)
	engine.mu.Lock()
	engine.accounts["a"] = &CoreAccount{ID: "a"}
	engine.mu.Unlock()
	quota := coreTestQuota(now, 10, 20)
	engine.finishRefreshSuccess("a", quota)
	account, _ := engine.accountByID("a")
	resetAt := account.Quota.FiveHour.ResetAt

	cfg := engine.Config()
	cfg.ResetProbeAfterResetDelay = 10 * time.Minute
	if err := engine.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	account, _ = engine.accountByID("a")
	if want := resetAt.Add(10 * time.Minute); !account.ProbeDueAt.Equal(want) {
		t.Fatalf("probe due not rescheduled after delay change: got %v want %v", account.ProbeDueAt, want)
	}

	cfg = engine.Config()
	cfg.EnableResetProbe = false
	if err := engine.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	account, _ = engine.accountByID("a")
	if !account.ProbeDueAt.IsZero() || !account.ProbeBaselineResetAt.IsZero() {
		t.Fatalf("probe schedule remained after disabling probe: due=%v baseline=%v", account.ProbeDueAt, account.ProbeBaselineResetAt)
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

func TestCoreManualQueuedRefreshBypassesAutomaticRefreshSwitch(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	host := &coreTestHost{
		auths:    []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9}},
		authJSON: map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access","account_id":"acct-a"}`)},
		quota:    pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: coreTestQuotaJSON()},
	}
	engine := newCoreTestEngine(t, host, now)
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	off := false
	if err := engine.SetAccountPreference("a", &off, nil, nil); err != nil {
		t.Fatal(err)
	}
	engine.RequestRefreshOne("a")
	engine.RunCycle()
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 1 || host.requests[0].Method != http.MethodGet || host.requests[0].URL != coreQuotaEndpoint {
		t.Fatalf("manual refresh requests=%v, want one quota GET", host.requests)
	}
}

func TestCoreProbePrecheckFailureNeverPosts(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 5, 0, 0, time.UTC)
	host := &coreTestHost{
		auths:    []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9}},
		authJSON: map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access","account_id":"acct-a"}`)},
		quota:    pluginapi.HTTPResponse{StatusCode: http.StatusInternalServerError},
	}
	engine := newCoreTestEngine(t, host, now)
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	baseline := now.Add(-5 * time.Minute)
	engine.mu.Lock()
	a := engine.accounts["a"]
	a.ProbeBaselineResetAt = baseline
	a.ProbeDueAt = now.Add(-time.Second)
	a.Quota = ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: baseline, Exhausted: true}}
	engine.mu.Unlock()
	if err := engine.ProbeAccount("a"); err == nil {
		t.Fatal("probe precheck failure returned nil error")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 1 || host.requests[0].Method != http.MethodGet {
		t.Fatalf("probe precheck requests=%v, want exactly one GET and no POST", host.requests)
	}
}

func TestCoreProbePostsOnlyForLazyFiveHourReset(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 5, 0, 0, time.UTC)
	baseline := now.Add(-5 * time.Minute)
	tests := []struct {
		name     string
		used5h   float64
		usedLong float64
		status   string
	}{
		{name: "five hour already available", used5h: 20, usedLong: 20, status: "precheck_five_hour_available"},
		{name: "long window exhausted", used5h: 100, usedLong: 100, status: "precheck_long_window_exhausted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"rate_limit": map[string]any{
				"primary_window":   map[string]any{"used_percent": tc.used5h, "limit_window_seconds": 18000, "reset_at": baseline.Format(time.RFC3339)},
				"secondary_window": map[string]any{"used_percent": tc.usedLong, "limit_window_seconds": 604800, "reset_at": now.Add(24 * time.Hour).Format(time.RFC3339)},
			}})
			if err != nil {
				t.Fatal(err)
			}
			host := &coreTestHost{
				auths:    []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9}},
				authJSON: map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access","account_id":"acct-a"}`)},
				quota:    pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body},
			}
			engine := newCoreTestEngine(t, host, now)
			if err := engine.SyncRoster(); err != nil {
				t.Fatal(err)
			}
			engine.mu.Lock()
			a := engine.accounts["a"]
			a.ProbeBaselineResetAt = baseline
			a.ProbeDueAt = now.Add(-time.Second)
			a.Quota = ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: baseline, Exhausted: true}}
			engine.mu.Unlock()
			if err := engine.ProbeAccount("a"); err != nil {
				t.Fatal(err)
			}
			host.mu.Lock()
			if len(host.requests) != 1 || host.requests[0].Method != http.MethodGet {
				host.mu.Unlock()
				t.Fatalf("probe requests=%v, want exactly one GET and no POST", host.requests)
			}
			host.mu.Unlock()
			account, _ := engine.accountByID("a")
			if account.ProbeStatus != tc.status {
				t.Fatalf("probe status=%q, want %q", account.ProbeStatus, tc.status)
			}
		})
	}
}

func TestCoreProbeRetryDueSurvivesRosterSync(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 5, 0, 0, time.UTC)
	host := &coreTestHost{auths: []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9}}}
	engine := newCoreTestEngine(t, host, now)
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	resetAt := now.Add(-5 * time.Minute)
	engine.mu.Lock()
	a := engine.accounts["a"]
	a.Quota = ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: resetAt, Exhausted: true}}
	a.ProbeBaselineResetAt = resetAt
	a.ProbeDueAt = now.Add(-time.Second)
	engine.mu.Unlock()
	engine.retryProbe("a", "rate_limited")
	want, _ := engine.accountByID("a")
	engine.mu.Lock()
	engine.accounts["a"].Quota.FiveHour.ResetAt = resetAt.Add(2 * time.Second)
	engine.mu.Unlock()
	if err := engine.SyncRoster(); err != nil {
		t.Fatal(err)
	}
	got, _ := engine.accountByID("a")
	if !got.ProbeDueAt.Equal(want.ProbeDueAt) {
		t.Fatalf("probe retry due changed across roster sync: got %v want %v", got.ProbeDueAt, want.ProbeDueAt)
	}
}

func TestCoreSuccessfulQuotaRecoveryClearsUsage429Ban(t *testing.T) {
	now := time.Date(2026, 8, 28, 14, 5, 0, 0, time.UTC)
	engine := newCoreTestEngine(t, &coreTestHost{}, now)
	engine.mu.Lock()
	engine.accounts["a"] = &CoreAccount{ID: "a", BanUntil: now.Add(time.Hour), BanReason: "usage_429"}
	engine.mu.Unlock()
	engine.finishRefreshSuccess("a", coreTestQuota(now, 0, 10))
	account, _ := engine.accountByID("a")
	if !account.BanUntil.IsZero() || account.BanReason != "" {
		t.Fatalf("usage_429 ban not cleared after quota recovery: until=%v reason=%q", account.BanUntil, account.BanReason)
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
