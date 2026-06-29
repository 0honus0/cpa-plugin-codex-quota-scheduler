# Adaptive Refresh And Dynamic UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace fixed quota polling with active-window due/retry refresh and update the management UI without hard page reloads.

**Architecture:** Keep the CPA security boundary unchanged: resource routes serve UI only, while all writes and privileged callbacks stay behind Management API routes. Add adaptive refresh metadata to account state, let `scheduler.pick` mark Codex activity, and let the refresher process only due/retry accounts while active. Rework the status page JavaScript to fetch Management API status JSON and re-render sections in place.

**Tech Stack:** Go 1.x, CLIProxyAPI v7 plugin SDK, `html/template`, browser `fetch`, standard Go tests.

---

## Working Directory

Use this isolated worktree for all implementation:

```powershell
C:\Users\Jeffery\Desktop\Projects\Personal\CPA-Plugin\cpa-plugin-codex-quota-scheduler\.worktrees\adaptive-refresh-ui
```

Do not implement on `main`.

## File Structure

- Modify `config.go`: new adaptive refresh settings, defaults, YAML decoding, normalization.
- Modify `models.go`: refresh metadata and account visibility fields.
- Modify `state.go`: active-window timestamp, account refresh metadata updates, due-account helpers.
- Modify `dispatch.go`: record Codex activity from scheduler requests and remove startup full refresh by default.
- Modify `refresh.go`: active-window worker, due/retry queue, failure classification, account-scoped refresh decisions.
- Modify `usage.go`: keep 429 behavior and mark no-reset failures due after the short pause.
- Modify `management.go`: settings payload, status payload, collapsed settings UI, dynamic page JS, no hard reload.
- Modify `disk_state.go`: persist new config and safe refresh metadata if needed.
- Modify tests in `config_test.go`, `state_test.go`, `refresh_test.go`, `dispatch_test.go`, `management_test.go`, `usage_test.go`.
- Modify `README.md`: document adaptive refresh defaults and the Management API boundary.

## Task 1: Adaptive Config Defaults

**Files:**
- Modify: `config.go`
- Modify: `config_test.go`
- Modify: `management.go`
- Modify: `management_test.go`

- [ ] **Step 1: Write failing config default test**

Add to `config_test.go`:

```go
func TestDefaultConfigAdaptiveRefreshDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RefreshActiveWindow != time.Hour {
		t.Fatalf("RefreshActiveWindow = %s, want 1h", cfg.RefreshActiveWindow)
	}
	if cfg.RefreshAfterResetDelay != time.Minute {
		t.Fatalf("RefreshAfterResetDelay = %s, want 1m", cfg.RefreshAfterResetDelay)
	}
	if got := cfg.RefreshRetryDelays; !reflect.DeepEqual(got, []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}) {
		t.Fatalf("RefreshRetryDelays = %#v, want 1m,5m,15m", got)
	}
	if cfg.RefreshOnStartup {
		t.Fatal("RefreshOnStartup = true, want false")
	}
	if cfg.CircuitFailureThreshold != 5 {
		t.Fatalf("CircuitFailureThreshold = %d, want 5", cfg.CircuitFailureThreshold)
	}
	if cfg.CircuitOpenDuration != 30*time.Minute {
		t.Fatalf("CircuitOpenDuration = %s, want 30m", cfg.CircuitOpenDuration)
	}
	if cfg.CircuitHalfOpenSuccessThreshold != 2 {
		t.Fatalf("CircuitHalfOpenSuccessThreshold = %d, want 2", cfg.CircuitHalfOpenSuccessThreshold)
	}
	if cfg.MaxLogEntries != 200 {
		t.Fatalf("MaxLogEntries = %d, want 200", cfg.MaxLogEntries)
	}
}
```

Ensure `config_test.go` imports `reflect` if it does not already.

- [ ] **Step 2: Run test to verify it fails**

Run:

```powershell
go test ./... -run TestDefaultConfigAdaptiveRefreshDefaults
```

Expected: FAIL with missing fields such as `cfg.RefreshActiveWindow undefined`.

- [ ] **Step 3: Add config fields and defaults**

In `config.go`, add fields to `Config`:

```go
RefreshActiveWindow    time.Duration
RefreshAfterResetDelay time.Duration
RefreshRetryDelays     []time.Duration
RefreshOnStartup       bool
```

Add fields to `rawConfig`:

```go
RefreshActiveWindow    string `yaml:"refresh_active_window"`
RefreshAfterResetDelay string `yaml:"refresh_after_reset_delay"`
RefreshRetryDelays     string `yaml:"refresh_retry_delays"`
RefreshOnStartup       *bool  `yaml:"refresh_on_startup"`
```

Update `DefaultConfig()`:

```go
RefreshActiveWindow:                 time.Hour,
RefreshAfterResetDelay:              time.Minute,
RefreshRetryDelays:                  []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
RefreshOnStartup:                    false,
CircuitFailureThreshold:             5,
CircuitOpenDuration:                 30 * time.Minute,
CircuitHalfOpenSuccessThreshold:     2,
MaxLogEntries:                       200,
```

Keep `QuotaRefreshInterval: 30 * time.Minute` for config compatibility, but do not use it as the polling driver in the implementation tasks below.

- [ ] **Step 4: Normalize slices safely**

In `NormalizeConfig`, ensure defaults are copied:

```go
if cfg.RefreshActiveWindow <= 0 {
	cfg.RefreshActiveWindow = defaults.RefreshActiveWindow
}
if cfg.RefreshAfterResetDelay <= 0 {
	cfg.RefreshAfterResetDelay = defaults.RefreshAfterResetDelay
}
if len(cfg.RefreshRetryDelays) == 0 {
	cfg.RefreshRetryDelays = append([]time.Duration(nil), defaults.RefreshRetryDelays...)
} else {
	cfg.RefreshRetryDelays = normalizeRetryDelays(cfg.RefreshRetryDelays, defaults.RefreshRetryDelays)
}
```

Add helper in `config.go`:

```go
func normalizeRetryDelays(values, fallback []time.Duration) []time.Duration {
	normalized := make([]time.Duration, 0, len(values))
	for _, value := range values {
		if value > 0 {
			normalized = append(normalized, value)
		}
	}
	if len(normalized) == 0 {
		return append([]time.Duration(nil), fallback...)
	}
	return normalized
}
```

- [ ] **Step 5: Decode YAML settings**

In `DecodeConfig`, parse the new fields:

```go
if decoded.RefreshActiveWindow != "" {
	d, err := time.ParseDuration(decoded.RefreshActiveWindow)
	if err != nil {
		return Config{}, fmt.Errorf("refresh_active_window: %w", err)
	}
	if d <= 0 {
		return Config{}, fmt.Errorf("refresh_active_window must be positive")
	}
	cfg.RefreshActiveWindow = d
}
if decoded.RefreshAfterResetDelay != "" {
	d, err := time.ParseDuration(decoded.RefreshAfterResetDelay)
	if err != nil {
		return Config{}, fmt.Errorf("refresh_after_reset_delay: %w", err)
	}
	if d <= 0 {
		return Config{}, fmt.Errorf("refresh_after_reset_delay must be positive")
	}
	cfg.RefreshAfterResetDelay = d
}
if decoded.RefreshRetryDelays != "" {
	delays, err := parseDurationList(decoded.RefreshRetryDelays)
	if err != nil {
		return Config{}, fmt.Errorf("refresh_retry_delays: %w", err)
	}
	cfg.RefreshRetryDelays = delays
}
if decoded.RefreshOnStartup != nil {
	cfg.RefreshOnStartup = *decoded.RefreshOnStartup
}
```

Add helper:

```go
func parseDurationList(raw string) ([]time.Duration, error) {
	parts := strings.Split(raw, ",")
	delays := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		d, err := time.ParseDuration(part)
		if err != nil {
			return nil, err
		}
		if d <= 0 {
			return nil, fmt.Errorf("duration must be positive")
		}
		delays = append(delays, d)
	}
	if len(delays) == 0 {
		return nil, fmt.Errorf("at least one duration is required")
	}
	return delays, nil
}
```

- [ ] **Step 6: Write settings payload tests**

Add to `management_test.go`:

```go
func TestSettingsPayloadIncludesAdaptiveRefresh(t *testing.T) {
	cfg := DefaultConfig()
	payload := SettingsFromConfig(cfg)
	if payload.RefreshActiveWindow != "1h0m0s" {
		t.Fatalf("RefreshActiveWindow = %q, want 1h0m0s", payload.RefreshActiveWindow)
	}
	if payload.RefreshAfterResetDelay != "1m0s" {
		t.Fatalf("RefreshAfterResetDelay = %q, want 1m0s", payload.RefreshAfterResetDelay)
	}
	if payload.RefreshRetryDelays != "1m0s,5m0s,15m0s" {
		t.Fatalf("RefreshRetryDelays = %q, want 1m0s,5m0s,15m0s", payload.RefreshRetryDelays)
	}
	if payload.RefreshOnStartup {
		t.Fatal("RefreshOnStartup = true, want false")
	}
}
```

- [ ] **Step 7: Add settings payload fields**

In `management.go`, add to `SettingsPayload`:

```go
RefreshActiveWindow    string `json:"refresh_active_window"`
RefreshAfterResetDelay string `json:"refresh_after_reset_delay"`
RefreshRetryDelays     string `json:"refresh_retry_delays"`
RefreshOnStartup       bool   `json:"refresh_on_startup"`
```

Add helper:

```go
func formatDurationList(values []time.Duration) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value > 0 {
			parts = append(parts, value.String())
		}
	}
	return strings.Join(parts, ",")
}
```

Populate fields in `SettingsFromConfig`, and parse them in `ConfigFromSettings` using `time.ParseDuration` and `parseDurationList`.

- [ ] **Step 8: Run tests**

Run:

```powershell
go test ./... -run "TestDefaultConfigAdaptiveRefreshDefaults|TestSettingsPayloadIncludesAdaptiveRefresh|TestDecodeConfig"
```

Expected: PASS.

- [ ] **Step 9: Commit**

```powershell
git add config.go config_test.go management.go management_test.go
git commit -m "feat: add adaptive refresh config"
```

## Task 2: Refresh State Metadata

**Files:**
- Modify: `models.go`
- Modify: `state.go`
- Modify: `state_test.go`

- [ ] **Step 1: Write failing state tests**

Add to `state_test.go`:

```go
func TestRecordCodexActivityControlsActiveWindow(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	if store.RefreshActive(now) {
		t.Fatal("RefreshActive before activity = true, want false")
	}
	store.RecordCodexActivity(now)
	if !store.RefreshActive(now.Add(59 * time.Minute)) {
		t.Fatal("RefreshActive inside 1h window = false, want true")
	}
	if store.RefreshActive(now.Add(time.Hour + time.Second)) {
		t.Fatal("RefreshActive after 1h window = true, want false")
	}
}

func TestRecordRefreshFailureSchedulesRetry(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	account := AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}
	store.UpsertQuota(account)
	updated, ok := store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureTransient, "request failed", now)
	if !ok {
		t.Fatal("RecordRefreshFailure returned ok=false")
	}
	if updated.Refresh.RetryAttempt != 1 {
		t.Fatalf("RetryAttempt = %d, want 1", updated.Refresh.RetryAttempt)
	}
	if !updated.Refresh.NextRetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("NextRetryAt = %s, want %s", updated.Refresh.NextRetryAt, now.Add(time.Minute))
	}
	if updated.Refresh.AuthFailure {
		t.Fatal("AuthFailure = true, want false")
	}
}

func TestRecordRefreshAuthFailureStopsRetry(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	updated, ok := store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureAuth, "please re-login", now)
	if !ok {
		t.Fatal("RecordRefreshFailure returned ok=false")
	}
	if !updated.Refresh.AuthFailure {
		t.Fatal("AuthFailure = false, want true")
	}
	if !updated.Refresh.NextRetryAt.IsZero() {
		t.Fatalf("NextRetryAt = %s, want zero", updated.Refresh.NextRetryAt)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```powershell
go test ./... -run "TestRecordCodexActivityControlsActiveWindow|TestRecordRefreshFailureSchedulesRetry|TestRecordRefreshAuthFailureStopsRetry"
```

Expected: FAIL with missing types and methods.

- [ ] **Step 3: Add refresh metadata types**

In `models.go`:

```go
type RefreshFailureKind string

const (
	RefreshFailureNone      RefreshFailureKind = ""
	RefreshFailureTransient RefreshFailureKind = "transient"
	RefreshFailureAuth      RefreshFailureKind = "auth"
	RefreshFailureLocal     RefreshFailureKind = "local"
)

type AccountRefreshState struct {
	LastFailureKind RefreshFailureKind
	RetryAttempt    int
	NextRetryAt     time.Time
	AuthFailure     bool
	DueReason       string
	LastFailureAt   time.Time
}
```

Add to `AccountState`:

```go
Refresh AccountRefreshState
```

Add to `PluginState`:

```go
lastCodexActivityAt time.Time
```

- [ ] **Step 4: Implement activity and failure methods**

In `state.go`, add:

```go
func (s *PluginState) RecordCodexActivity(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastCodexActivityAt.IsZero() || now.After(s.lastCodexActivityAt) {
		s.lastCodexActivityAt = now
	}
}

func (s *PluginState) RefreshActive(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := NormalizeConfig(s.cfg)
	return !s.lastCodexActivityAt.IsZero() && !now.After(s.lastCodexActivityAt.Add(cfg.RefreshActiveWindow))
}

func (s *PluginState) RecordRefreshFailure(authID, authIndex string, kind RefreshFailureKind, message string, now time.Time) (AccountState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, account, ok := s.findAccountLocked(authID, authIndex)
	if !ok {
		if authID == "" {
			return AccountState{}, false
		}
		key = "auth:" + authID
		account = AccountState{AuthID: authID, AuthIndex: authIndex, Provider: "codex"}
	}
	account.LastError = message
	account.Refresh.LastFailureKind = kind
	account.Refresh.LastFailureAt = now
	if kind == RefreshFailureAuth || kind == RefreshFailureLocal {
		account.Refresh.AuthFailure = kind == RefreshFailureAuth
		account.Refresh.NextRetryAt = time.Time{}
	} else {
		account.Refresh.AuthFailure = false
		account.Refresh.RetryAttempt++
		account.Refresh.NextRetryAt = now.Add(retryDelayForAttempt(NormalizeConfig(s.cfg), account.Refresh.RetryAttempt))
	}
	s.accounts[key] = account
	return cloneAccountState(account), true
}
```

Add helper:

```go
func retryDelayForAttempt(cfg Config, attempt int) time.Duration {
	delays := NormalizeConfig(cfg).RefreshRetryDelays
	if len(delays) == 0 {
		return time.Minute
	}
	if attempt <= 0 {
		return delays[0]
	}
	index := attempt - 1
	if index >= len(delays) {
		index = len(delays) - 1
	}
	return delays[index]
}
```

- [ ] **Step 5: Clear refresh failure on success**

In `PluginState.UpsertQuota`, before storing a successful quota account, clear retry state when `LastSuccessAt` is non-zero and `LastError` is empty:

```go
if !account.LastSuccessAt.IsZero() && account.LastError == "" {
	account.Refresh = AccountRefreshState{}
}
```

- [ ] **Step 6: Run tests**

```powershell
go test ./... -run "TestRecordCodexActivityControlsActiveWindow|TestRecordRefreshFailureSchedulesRetry|TestRecordRefreshAuthFailureStopsRetry"
```

Expected: PASS.

- [ ] **Step 7: Commit**

```powershell
git add models.go state.go state_test.go
git commit -m "feat: track adaptive refresh state"
```

## Task 3: Startup And Activity-Driven Refresh

**Files:**
- Modify: `dispatch.go`
- Modify: `refresh.go`
- Modify: `dispatch_test.go`
- Modify: `refresh_test.go`

- [ ] **Step 1: Write startup behavior test**

Add to `refresh_test.go`:

```go
func TestStartDoesNotRefreshOnStartupByDefault(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
	}
	store := NewPluginState(DefaultConfig())
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	refresher.Start()
	defer refresher.Stop()
	time.Sleep(50 * time.Millisecond)
	if got := host.httpCallCount(); got != 0 {
		t.Fatalf("http calls = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Write scheduler activity test**

Add to `dispatch_test.go`:

```go
func TestSchedulerPickRecordsCodexActivity(t *testing.T) {
	previousState := globalState
	globalState = NewPluginState(DefaultConfig())
	defer func() { globalState = previousState }()

	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{{
			ID: "auth-1", Provider: "codex", Priority: 1,
		}},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatalf("handleSchedulerPick returned error: %v", err)
	}
	if !globalState.RefreshActive(time.Now()) {
		t.Fatal("RefreshActive = false after Codex scheduler pick, want true")
	}
}
```

- [ ] **Step 3: Run tests to verify failure**

```powershell
go test ./... -run "TestStartDoesNotRefreshOnStartupByDefault|TestSchedulerPickRecordsCodexActivity"
```

Expected: startup test may fail because `startGlobalRefresher` currently enqueues `RefreshSoon`; scheduler test fails until activity recording is added.

- [ ] **Step 4: Stop immediate startup refresh**

In `dispatch.go`, change `startGlobalRefresher`:

```go
func startGlobalRefresher() {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher == nil {
		return
	}
	refresher.Start()
	if globalState.Config().RefreshOnStartup {
		refresher.RefreshSoon()
	}
}
```

- [ ] **Step 5: Record Codex activity from scheduler**

In `handleSchedulerPick`, after decoding and before `PickCodexAccount`:

```go
if requestIncludesCodex(req) {
	globalState.RecordCodexActivity(now)
	refreshGlobalRefresherDueSoon()
}
```

Add helper:

```go
func refreshGlobalRefresherDueSoon() {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher != nil {
		refresher.RefreshDueSoon()
	}
}
```

- [ ] **Step 6: Add `RefreshDueSoon` as active-window gated refresh**

In `refresh.go`:

```go
func (r *QuotaRefresher) RefreshDueSoon() {
	if r.state == nil || !r.state.RefreshActive(r.now()) {
		return
	}
	r.RefreshSoon()
}
```

This initially reuses `RefreshSoon`; Task 4 narrows it to due accounts.

- [ ] **Step 7: Run tests**

```powershell
go test ./... -run "TestStartDoesNotRefreshOnStartupByDefault|TestSchedulerPickRecordsCodexActivity|TestRefreshSoonDoesNotOverlapRefreshes"
```

Expected: PASS.

- [ ] **Step 8: Commit**

```powershell
git add dispatch.go dispatch_test.go refresh.go refresh_test.go
git commit -m "feat: gate refresh by codex activity"
```

## Task 4: Due Account Selection

**Files:**
- Modify: `state.go`
- Modify: `refresh.go`
- Modify: `state_test.go`
- Modify: `refresh_test.go`

- [ ] **Step 1: Write due helper tests**

Add to `state_test.go`:

```go
func TestAccountRefreshDueReasons(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	if due, reason := accountRefreshDue(AccountState{AuthID: "a"}, cfg, now); !due || reason != "never_refreshed" {
		t.Fatalf("never refreshed due=%v reason=%q, want true never_refreshed", due, reason)
	}
	stale := AccountState{AuthID: "a", LastSuccessAt: now.Add(-6 * time.Hour)}
	if due, reason := accountRefreshDue(stale, cfg, now); !due || reason != "stale" {
		t.Fatalf("stale due=%v reason=%q, want true stale", due, reason)
	}
	retry := AccountState{AuthID: "a", LastSuccessAt: now.Add(-time.Hour)}
	retry.Refresh.NextRetryAt = now.Add(-time.Second)
	if due, reason := accountRefreshDue(retry, cfg, now); !due || reason != "retry_due" {
		t.Fatalf("retry due=%v reason=%q, want true retry_due", due, reason)
	}
	authFailed := AccountState{AuthID: "a", LastSuccessAt: now.Add(-6 * time.Hour)}
	authFailed.Refresh.AuthFailure = true
	if due, reason := accountRefreshDue(authFailed, cfg, now); due || reason != "auth_failure" {
		t.Fatalf("auth failure due=%v reason=%q, want false auth_failure", due, reason)
	}
}
```

- [ ] **Step 2: Write refresh scoped test**

Add to `refresh_test.go`:

```go
func TestRefreshDueOnceSkipsFreshAccounts(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "fresh", AuthIndex: "idx-fresh", Provider: "codex"},
			{ID: "stale", AuthIndex: "idx-stale", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-fresh": json.RawMessage(`{"access_token":"access-fresh","id_token":"` + idToken + `"}`),
			"idx-stale": json.RawMessage(`{"access_token":"access-stale","id_token":"` + idToken + `"}`),
		},
		httpBody:   []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		httpStatus: http.StatusOK,
	}
	store := NewPluginState(DefaultConfig())
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{AuthID: "fresh", AuthIndex: "idx-fresh", Provider: "codex", LastSuccessAt: now.Add(-time.Hour)})
	store.UpsertQuota(AccountState{AuthID: "stale", AuthIndex: "idx-stale", Provider: "codex", LastSuccessAt: now.Add(-6 * time.Hour)})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce returned error: %v", err)
	}
	if got := host.httpCallCount(); got != 2 {
		t.Fatalf("http calls = %d, want 2 for one quota and one reset-credit request", got)
	}
}
```

- [ ] **Step 3: Run tests to verify failure**

```powershell
go test ./... -run "TestAccountRefreshDueReasons|TestRefreshDueOnceSkipsFreshAccounts"
```

Expected: FAIL with missing `accountRefreshDue` and `RefreshDueOnce`.

- [ ] **Step 4: Implement due helper**

Add in `state.go`:

```go
func accountRefreshDue(account AccountState, cfg Config, now time.Time) (bool, string) {
	cfg = NormalizeConfig(cfg)
	if account.Refresh.AuthFailure {
		return false, "auth_failure"
	}
	if account.LastSuccessAt.IsZero() {
		return true, "never_refreshed"
	}
	if now.Sub(account.LastSuccessAt) > cfg.StaleAfter {
		return true, "stale"
	}
	if !account.Refresh.NextRetryAt.IsZero() && !account.Refresh.NextRetryAt.After(now) {
		return true, "retry_due"
	}
	if resetDue(account.Quota.FiveHour, cfg.RefreshAfterResetDelay, now) {
		return true, "five_hour_reset_due"
	}
	if resetDue(account.Quota.LongWindow, cfg.RefreshAfterResetDelay, now) {
		return true, "long_window_reset_due"
	}
	if account.TemporaryExhausted && !account.TemporaryResetAt.IsZero() && !account.TemporaryResetAt.Add(cfg.RefreshAfterResetDelay).After(now) {
		return true, "temporary_reset_due"
	}
	return false, ""
}

func resetDue(window *QuotaWindow, delay time.Duration, now time.Time) bool {
	return window != nil && !window.ResetAt.IsZero() && !window.ResetAt.Add(delay).After(now)
}
```

Add method:

```go
func (s *PluginState) DueAccounts(now time.Time) []AccountState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := NormalizeConfig(s.cfg)
	accounts := make([]AccountState, 0)
	for _, account := range s.accounts {
		cloned := cloneAccountState(account)
		if due, reason := accountRefreshDue(cloned, cfg, now); due {
			cloned.Refresh.DueReason = reason
			accounts = append(accounts, cloned)
		}
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Priority != accounts[j].Priority {
			return accounts[i].Priority > accounts[j].Priority
		}
		return accounts[i].AuthID < accounts[j].AuthID
	})
	return accounts
}
```

- [ ] **Step 5: Implement due refresh**

In `refresh.go`, add:

```go
func (r *QuotaRefresher) RefreshDueOnce() error {
	if r.state == nil || !r.state.RefreshActive(r.now()) {
		return nil
	}
	due := r.state.DueAccounts(r.now())
	if len(due) == 0 {
		return nil
	}
	auths, err := r.host.ListAuths()
	if err != nil {
		return fmt.Errorf("list auths: %w", err)
	}
	dueIDs := make(map[string]struct{}, len(due))
	for _, account := range due {
		if account.AuthID != "" {
			dueIDs[account.AuthID] = struct{}{}
		}
	}
	eligible := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	for _, auth := range auths {
		if _, ok := dueIDs[auth.ID]; ok && isRefreshEligible(auth) {
			eligible = append(eligible, auth)
		}
	}
	return r.refreshAuthEntries(eligible)
}
```

Extract the worker body from `RefreshOnce` into:

```go
func (r *QuotaRefresher) refreshAuthEntries(eligible []pluginapi.HostAuthFileEntry) error {
	if len(eligible) == 0 {
		return nil
	}
	concurrency := r.state.Config().MaxRefreshConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(eligible) {
		concurrency = len(eligible)
	}
	jobs := make(chan pluginapi.HostAuthFileEntry)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for auth := range jobs {
				r.refreshAuth(auth)
			}
		}()
	}
	for _, auth := range eligible {
		jobs <- auth
	}
	close(jobs)
	wg.Wait()
	return nil
}
```

Update `RefreshOnce` to list all eligible auths and call `refreshAuthEntries`.

- [ ] **Step 6: Make `RefreshDueSoon` use due refresh**

Change `RefreshDueSoon` to call an async helper that runs `RefreshDueOnce` with the same non-overlap guard as `RefreshSoon`.

- [ ] **Step 7: Run tests**

```powershell
go test ./... -run "TestAccountRefreshDueReasons|TestRefreshDueOnceSkipsFreshAccounts|TestRefreshOnceLoadsCodexAuthAndQuota"
```

Expected: PASS.

- [ ] **Step 8: Commit**

```powershell
git add state.go state_test.go refresh.go refresh_test.go
git commit -m "feat: refresh only due accounts"
```

## Task 5: Failure Classification And Retry

**Files:**
- Modify: `refresh.go`
- Modify: `refresh_test.go`
- Modify: `management.go`
- Modify: `management_test.go`

- [ ] **Step 1: Write failure classification tests**

Add to `refresh_test.go`:

```go
func TestRefreshFailure401MarksAuthFailure(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	host, store := fakeRefreshFailureHarness(t, http.StatusUnauthorized, `{"error":"expired"}`, now)
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	account := store.Snapshot(now).Accounts[0]
	if !account.Refresh.AuthFailure {
		t.Fatal("AuthFailure = false, want true")
	}
	if !account.Refresh.NextRetryAt.IsZero() {
		t.Fatalf("NextRetryAt = %s, want zero", account.Refresh.NextRetryAt)
	}
}

func TestRefreshFailure403SchedulesRetry(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	host, store := fakeRefreshFailureHarness(t, http.StatusForbidden, `{"error":"forbidden"}`, now)
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	account := store.Snapshot(now).Accounts[0]
	if account.Refresh.AuthFailure {
		t.Fatal("AuthFailure = true, want false")
	}
	if !account.Refresh.NextRetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("NextRetryAt = %s, want %s", account.Refresh.NextRetryAt, now.Add(time.Minute))
	}
}
```

Add helper in `refresh_test.go` near existing fake host helpers:

```go
func fakeRefreshFailureHarness(t *testing.T, status int, body string, now time.Time) (*fakeHostClient, *PluginState) {
	t.Helper()
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpStatus: status,
		httpBody:   []byte(body),
	}
	store := NewPluginState(DefaultConfig())
	return host, store
}
```

- [ ] **Step 2: Run tests to verify failure**

```powershell
go test ./... -run "TestRefreshFailure401MarksAuthFailure|TestRefreshFailure403SchedulesRetry"
```

Expected: FAIL until refresh failures update `AccountRefreshState`.

- [ ] **Step 3: Implement failure kind classification**

In `refresh.go`, add:

```go
func refreshFailureKind(status int, err error) RefreshFailureKind {
	if status == http.StatusUnauthorized {
		return RefreshFailureAuth
	}
	if err != nil {
		return RefreshFailureTransient
	}
	return RefreshFailureTransient
}
```

Change non-2xx quota response handling in `refreshAuth`:

```go
message := fmt.Sprintf("quota request returned status %d: response body: %s", resp.StatusCode, sanitizedBodySummary(resp.Body))
r.upsertRefreshFailure(account, redactWithCredentials(message, credentials), refreshFailureKind(resp.StatusCode, nil))
return
```

Change request error handling:

```go
r.upsertRefreshFailure(account, redactWithCredentials(fmt.Sprintf("quota request: %v", err), credentials), RefreshFailureTransient)
```

Change parse error handling:

```go
r.upsertRefreshFailure(account, redactWithCredentials(fmt.Sprintf("parse quota: %v", err), credentials), RefreshFailureTransient)
```

Change local credential extraction failures:

```go
r.upsertRefreshFailure(account, redactWithCredentials(fmt.Sprintf("extract credentials: %v", err), credentials), RefreshFailureLocal)
```

- [ ] **Step 4: Update `upsertRefreshFailure` signature**

Change:

```go
func (r *QuotaRefresher) upsertRefreshFailure(account AccountState, message string, kind RefreshFailureKind)
```

Inside it, after merging:

```go
updated, ok := r.state.RecordRefreshFailure(merged.AuthID, merged.AuthIndex, kind, message, r.now())
if ok {
	merged = updated
}
```

Preserve existing quota data by merging before recording failure.

- [ ] **Step 5: Show retry/auth status in management payload**

In `StatusAccount`, add:

```go
LastError         string `json:"last_error,omitempty"`
RefreshDueReason string `json:"refresh_due_reason,omitempty"`
NextRetryText    string `json:"next_retry_text,omitempty"`
AuthFailure      bool   `json:"auth_failure,omitempty"`
```

Populate from `AccountState.Refresh` and `LastError` in the account status conversion function.

- [ ] **Step 6: Run tests**

```powershell
go test ./... -run "TestRefreshFailure401MarksAuthFailure|TestRefreshFailure403SchedulesRetry|TestRefreshOnceFailurePreservesPriorSuccessfulQuota"
```

Expected: PASS.

- [ ] **Step 7: Commit**

```powershell
git add refresh.go refresh_test.go management.go management_test.go
git commit -m "feat: classify quota refresh failures"
```

## Task 6: No-Reset 429 Due Refresh

**Files:**
- Modify: `usage.go`
- Modify: `usage_test.go`

- [ ] **Step 1: Write usage feedback test**

Add to `usage_test.go`:

```go
func TestUsageLimitWithoutResetSchedulesShortPause(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	record := pluginapi.UsageRecord{
		Provider:  "codex",
		AuthID:    "auth-1",
		AuthIndex: "idx-1",
		Failed:    true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached"}}`,
		},
	}
	HandleUsageFeedback(store, record, now)
	account := store.Snapshot(now).Accounts[0]
	if !account.Circuit.NextProbeAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("NextProbeAt = %s, want %s", account.Circuit.NextProbeAt, now.Add(2*time.Minute))
	}
}
```

- [ ] **Step 2: Run test**

```powershell
go test ./... -run TestUsageLimitWithoutResetSchedulesShortPause
```

Expected: PASS if existing behavior already sets a 2-minute reset; otherwise FAIL and fix in Step 3.

- [ ] **Step 3: Ensure no-reset pause stays 2 minutes**

In `DetectQuotaFailure`, keep:

```go
event.ResetAt = now.Add(2 * time.Minute)
event.Reason = usageLimitNoResetReason
```

Ensure `RecordAccountFailure` sets `NextProbeAt` to at least that reset time.

- [ ] **Step 4: Run tests**

```powershell
go test ./... -run "TestUsageLimitWithoutResetSchedulesShortPause|TestDetectQuotaFailure"
```

Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add usage.go usage_test.go
git commit -m "test: cover no-reset quota feedback"
```

## Task 7: Dynamic Management UI

**Files:**
- Modify: `management.go`
- Modify: `management_test.go`

- [ ] **Step 1: Write UI regression tests**

Add to `management_test.go`:

```go
func TestStatusPageUsesCollapsedSettingsAndNoHardReload(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	page := renderStatusPageForTest(t, store)
	if !strings.Contains(page, `<details class="section collapsible" id="settingsPanel">`) &&
		!strings.Contains(page, `<details class="panel collapsible" id="settingsPanel">`) {
		t.Fatalf("page does not render settings as collapsed details")
	}
	if strings.Contains(page, `id="settingsPanel" open`) {
		t.Fatalf("settings panel is open by default")
	}
	if strings.Contains(page, "window.location.reload") {
		t.Fatalf("page still contains hard reload")
	}
	if !strings.Contains(page, `requestManagement('/status'`) {
		t.Fatalf("page does not fetch status for dynamic refresh")
	}
}
```

If no helper exists, add:

```go
func renderStatusPageForTest(t *testing.T, store *PluginState) string {
	t.Helper()
	resp := handleStatusRequest(store, pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/status"}, time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	return string(resp.Body)
}
```

- [ ] **Step 2: Run test to verify failure**

```powershell
go test ./... -run TestStatusPageUsesCollapsedSettingsAndNoHardReload
```

Expected: FAIL because page still contains `window.location.reload()` and settings are not a collapsed details panel.

- [ ] **Step 3: Rework settings markup**

In `statusTemplateV2`, replace the settings `<div class="section">` with:

```html
<details class="section collapsible" id="settingsPanel">
<summary>
  <span class="summary-text">
    <span class="summary-title" data-i18n="settings.title">调度设置</span>
    <span class="summary-subtitle" data-i18n="settings.summary">默认配置已经设置好，正常情况下不需要手动调整。</span>
  </span>
</summary>
<div class="collapsible-body">
  <!-- existing settings fields except Refresh quota -->
</div>
</details>
```

Move `refreshQuota` button outside the details panel, near the Management key or top toolbar:

```html
<div class="actions primary-actions">
  <button id="refreshQuota" type="button" class="secondary" data-i18n="actions.refreshQuota">刷新额度</button>
</div>
```

Keep Save Settings, Export Config, and Import Config inside the details panel.

- [ ] **Step 4: Add dynamic status fetch**

In the page script, replace immutable `const STATUS={{json .}};` with:

```js
let STATUS={{json .}};
```

Add:

```js
async function refreshStatus(){
  const data=await requestManagement('/status',{query:{format:'json'}});
  STATUS=data;
  rebuildDerivedState();
  fillSettings();
  renderAccounts(STATUS.accounts||[]);
  renderLogs(STATUS.logs||[]);
  applyLocale();
}
```

Add `rebuildDerivedState`:

```js
function rebuildDerivedState(){
  accountsByID.clear();
  groupsByID.clear();
  for(const account of STATUS.accounts||[]){
    accountsByID.set(account.auth_id,account);
    if(account.group_id){groupsByID.set(account.group_id,{name:account.group||'',notes:account.group_notes||''})}
  }
  for(const group of STATUS.groups||[]){
    if(group.id){groupsByID.set(group.id,{name:group.name||'',notes:group.notes||''})}
  }
}
```

Change `accountsByID` and `groupsByID` declarations from `const new Map(...)` to mutable empty maps:

```js
const accountsByID=new Map();
const groupsByID=new Map();
```

- [ ] **Step 5: Add account card rendering function**

Create `renderAccounts(accounts)` in the page script that rebuilds `section.queue` from JSON data. Use DOM APIs and `textContent`; do not concatenate untrusted account fields into HTML. At minimum render the same visible information as the template card: rank, title/alias, auth ID, badges, cache age, reset windows, notes, and card actions.

The implementation must attach event handlers after creating buttons:

```js
refreshButton.addEventListener('click',()=>refreshOneQuota(account.auth_id||''));
editButton.addEventListener('click',()=>openEdit(account.auth_id||''));
```

- [ ] **Step 6: Remove hard reload calls**

Delete `schedulePageRefresh`. Update action handlers:

```js
async function saveSettings(){
  try{
    const payload=collectSettingsPayload();
    await requestManagement('/settings',{method:'PUT',body:payload});
    showNotice(t('notice.settingsSaved'),false);
    await refreshStatus();
  }catch(error){showNotice(error.message||String(error),true)}
}
```

For `refreshQuota` and `refreshOneQuota`, call `refreshStatus()` once immediately after the accepted response, then poll:

```js
async function pollStatus(times, delayMs){
  for(let i=0;i<times;i++){
    await new Promise((resolve)=>window.setTimeout(resolve,delayMs));
    await refreshStatus();
  }
}
```

Use `pollStatus(3, 1200)` after refresh requests.

- [ ] **Step 7: Update translations**

Add translation keys:

```js
'settings.summary':'Defaults are already configured; normally no manual changes are needed.',
'settings.refreshActiveWindow':'Refresh active window',
'settings.refreshAfterResetDelay':'Refresh after reset delay',
'settings.refreshRetryDelays':'Refresh retry delays',
'settings.refreshOnStartup':'Refresh on startup'
```

Add equivalent Chinese strings using readable Chinese text.

- [ ] **Step 8: Run UI tests**

```powershell
go test ./... -run "TestStatusPageUsesCollapsedSettingsAndNoHardReload|TestHandleManagementResourceRejectsWriteActions|TestQuotaEndpoint"
```

Expected: PASS.

- [ ] **Step 9: Commit**

```powershell
git add management.go management_test.go
git commit -m "feat: update management ui dynamically"
```

## Task 8: Security Boundary Regression

**Files:**
- Modify: `management_test.go`
- Modify: `README.md`

- [ ] **Step 1: Add resource route regression test**

Add to `management_test.go`:

```go
func TestResourceRouteCannotTriggerActions(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	req := pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/codex-quota-scheduler/status?action=refresh",
	}
	resp := HandleManagementRequest(store, req, time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC))
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK && strings.Contains(string(resp.Body), `"ok":true`) {
		t.Fatalf("resource action unexpectedly succeeded: status=%d body=%s", resp.StatusCode, string(resp.Body))
	}
}
```

- [ ] **Step 2: Add quota endpoint import regression test**

Add to `management_test.go`:

```go
func TestImportRejectsNonChatGPTQuotaEndpoint(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	body := []byte(`{"settings":{"quota_endpoint":"https://example.test/steal"}}`)
	req := pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/import", Body: body}
	resp := HandleManagementRequest(store, req, time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC))
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want error for non-ChatGPT quota endpoint", resp.StatusCode)
	}
}
```

- [ ] **Step 3: Run tests to verify behavior**

```powershell
go test ./... -run "TestResourceRouteCannotTriggerActions|TestImportRejectsNonChatGPTQuotaEndpoint"
```

Expected: PASS after any needed import validation adjustments.

- [ ] **Step 4: Update README**

In `README.md`, update the settings example to include:

```yaml
stale_after: 5h
refresh_active_window: 1h
refresh_after_reset_delay: 1m
refresh_retry_delays: 1m,5m,15m
refresh_on_startup: false
max_refresh_concurrency: 1
circuit_failure_threshold: 5
circuit_open_duration: 30m
circuit_half_open_success_threshold: 2
max_log_entries: 200
log_retention: 24h
```

Add a Management API security note:

```markdown
The resource page under `/v0/resource/plugins/codex-quota-scheduler/status`
serves UI content only. Settings, import/export, annotations, logs, and refresh
actions use `/v0/management/plugins/codex-quota-scheduler/...` and require the
CPA Management key. The quota endpoint is restricted to
`https://chatgpt.com/backend-api/wham/usage`.
```

- [ ] **Step 5: Commit**

```powershell
git add management_test.go README.md
git commit -m "test: protect management resource boundary"
```

## Task 9: Full Verification

**Files:**
- No new files unless prior tasks reveal a required test fixture.

- [ ] **Step 1: Run full test suite**

```powershell
go test ./...
```

Expected: PASS.

- [ ] **Step 2: Inspect resource/action strings**

```powershell
rg -n "status\\?action|window\\.location\\.reload|quota_endpoint" .
```

Expected:

- no `status?action` implementation paths;
- no `window.location.reload` in UI script;
- `quota_endpoint` appears only in config, settings display/import validation, tests, README, and docs.

- [ ] **Step 3: Build plugin**

```powershell
go build -buildmode=c-shared -o dist/codex-quota-scheduler.dll .
```

Expected: command exits 0 and writes `dist/codex-quota-scheduler.dll`.

- [ ] **Step 4: Final status**

```powershell
git status --short
git log --oneline -8
```

Expected: clean working tree, with task commits on `codex/adaptive-refresh-ui`.

- [ ] **Step 5: Commit build docs only if needed**

Do not commit `dist/` unless the release workflow requires it. If no files changed after verification, skip this step.

## Self-Review

- Spec coverage: adaptive config, active window, due/retry refresh, 401 vs 403 classification, 429 feedback, dynamic UI, collapsed settings, resource/Management boundary, and endpoint restriction are covered by tasks.
- Placeholder scan: this plan contains no placeholder markers or unspecified implementation steps.
- Type consistency: new names are `RefreshActiveWindow`, `RefreshAfterResetDelay`, `RefreshRetryDelays`, `RefreshOnStartup`, `AccountRefreshState`, `RefreshFailureKind`, `RecordCodexActivity`, `RefreshActive`, `DueAccounts`, `RefreshDueOnce`, and `RefreshDueSoon`.
