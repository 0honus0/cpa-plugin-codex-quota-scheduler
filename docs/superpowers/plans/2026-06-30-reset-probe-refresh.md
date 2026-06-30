# Reset Probe Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in per-account Codex compact probe that starts lazy reset windows after cached reset times mature.

**Architecture:** Keep normal quota refresh on `host.http.do` and add a small reset-probe state machine to each cached account. Normal reset refresh may happen after one minute, but probe classification waits for an internal 10-minute maturity delay and uses a 3-minute lazy-reset threshold. The management UI exposes only a default-off checkbox and a visible warning; probe timing constants remain internal.

**Tech Stack:** Go 1.x, CLIProxyAPI v7 plugin SDK, `host.http.do`, standard Go tests, `html/template`.

---

## Working Directory

Implementation should use a worktree created at execution time with
`superpowers:using-git-worktrees`, for example:

```powershell
C:\Users\Jeffery\Desktop\Projects\Personal\CPA-Plugin\cpa-plugin-codex-quota-scheduler\.worktrees\reset-probe-refresh
```

Do not implement on `main`.

## File Structure

- Modify `config.go`: add `EnableResetProbe`, YAML decoding, normalization, and settings conversion support.
- Modify `models.go`: add `ResetProbeState` and `ResetProbeStatus` fields to `AccountState`.
- Create `probe.go`: internal constants, lazy-reset detection helpers, probe payload, usage-evidence parser, and probe-state transitions.
- Modify `state.go`: clone probe state, include pending probe due times in `DueAccounts` and `NextRefreshDueAt`.
- Modify `refresh.go`: schedule pending probes from old reset windows, execute due probe checks, send compact probe through `host.http.do`, and post-probe refresh quota.
- Modify `management.go`: render checkbox, visible warning, status fields, and settings payload.
- Modify tests in `config_test.go`, `state_test.go`, `refresh_test.go`, and `management_test.go`.

## Task 1: Config Flag

**Files:**
- Modify: `config.go`
- Modify: `config_test.go`
- Modify: `management.go`
- Modify: `management_test.go`

- [ ] **Step 1: Write failing config tests**

Add to `config_test.go`:

```go
func TestDefaultConfigDisablesResetProbe(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.EnableResetProbe {
		t.Fatal("EnableResetProbe = true, want false")
	}
}

func TestDecodeConfigEnableResetProbe(t *testing.T) {
	cfg, err := DecodeConfig([]byte("enable_reset_probe: true\n"))
	if err != nil {
		t.Fatalf("DecodeConfig returned error: %v", err)
	}
	if !cfg.EnableResetProbe {
		t.Fatal("EnableResetProbe = false, want true")
	}
}
```

Add to `management_test.go`:

```go
func TestSettingsPayloadIncludesResetProbeFlag(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	payload := SettingsFromConfig(cfg)
	if !payload.EnableResetProbe {
		t.Fatal("EnableResetProbe = false, want true")
	}

	roundTrip, err := ConfigFromSettings(SettingsPayload{EnableResetProbe: true})
	if err != nil {
		t.Fatalf("ConfigFromSettings returned error: %v", err)
	}
	if !roundTrip.EnableResetProbe {
		t.Fatal("roundTrip EnableResetProbe = false, want true")
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run:

```powershell
go test ./... -run "TestDefaultConfigDisablesResetProbe|TestDecodeConfigEnableResetProbe|TestSettingsPayloadIncludesResetProbeFlag"
```

Expected: FAIL with missing `EnableResetProbe` fields.

- [ ] **Step 3: Add config field and YAML decoding**

In `config.go`, add to `Config`:

```go
EnableResetProbe bool
```

Add to `rawConfig`:

```go
EnableResetProbe *bool `yaml:"enable_reset_probe"`
```

In `DefaultConfig()`, add:

```go
EnableResetProbe: false,
```

In `DecodeConfig`, after `EnableUsageFeedback` decoding:

```go
if decoded.EnableResetProbe != nil {
	cfg.EnableResetProbe = *decoded.EnableResetProbe
}
```

- [ ] **Step 4: Add settings payload support**

In `management.go`, add to `SettingsPayload`:

```go
EnableResetProbe bool `json:"enable_reset_probe"`
```

In `SettingsFromConfig`, set:

```go
EnableResetProbe: cfg.EnableResetProbe,
```

In `ConfigFromSettings`, set:

```go
cfg.EnableResetProbe = payload.EnableResetProbe
```

- [ ] **Step 5: Run tests**

Run:

```powershell
go test ./... -run "TestDefaultConfigDisablesResetProbe|TestDecodeConfigEnableResetProbe|TestSettingsPayloadIncludesResetProbeFlag"
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add config.go config_test.go management.go management_test.go
git commit -m "feat: add reset probe setting"
```

## Task 2: Probe State And Helpers

**Files:**
- Modify: `models.go`
- Create: `probe.go`
- Modify: `state_test.go`

- [ ] **Step 1: Write failing helper tests**

Add to `state_test.go`:

```go
func TestResetProbeConstantsAvoidEarlyMisclassification(t *testing.T) {
	if resetProbeAfterResetDelay <= resetProbeCloseThreshold {
		t.Fatalf("resetProbeAfterResetDelay = %s must be greater than resetProbeCloseThreshold = %s", resetProbeAfterResetDelay, resetProbeCloseThreshold)
	}
	if resetProbeAfterResetDelay != 10*time.Minute {
		t.Fatalf("resetProbeAfterResetDelay = %s, want 10m", resetProbeAfterResetDelay)
	}
	if resetProbeCloseThreshold != 3*time.Minute {
		t.Fatalf("resetProbeCloseThreshold = %s, want 3m", resetProbeCloseThreshold)
	}
}

func TestLooksLikeLazyReset(t *testing.T) {
	now := time.Date(2026, 6, 30, 10, 10, 0, 0, time.UTC)
	seconds := int64(fiveHourSeconds)
	lazy := QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: now.Add(5 * time.Hour)}
	if !looksLikeLazyReset(now, lazy) {
		t.Fatal("lazy reset was not detected")
	}
	active := QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: now.Add(5*time.Hour - 10*time.Minute)}
	if looksLikeLazyReset(now, active) {
		t.Fatal("active reset was misclassified as lazy")
	}
}

func TestMonthlyProbeRequiresWindowSeconds(t *testing.T) {
	monthly := QuotaWindow{Kind: WindowMonthly, ResetAt: time.Now().Add(30 * 24 * time.Hour)}
	if _, ok := probeWindowDuration(monthly); ok {
		t.Fatal("monthly window without limit_window_seconds returned duration, want none")
	}
	seconds := int64(2592000)
	monthly.LimitWindowSeconds = &seconds
	d, ok := probeWindowDuration(monthly)
	if !ok || d != 30*24*time.Hour {
		t.Fatalf("probeWindowDuration = %s, %v; want 720h,true", d, ok)
	}
}

func TestResetProbeUsageEvidence(t *testing.T) {
	if !resetProbeUsageEvidence([]byte(`{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)) {
		t.Fatal("usage evidence not detected")
	}
	if resetProbeUsageEvidence([]byte(`{"id":"probe","usage":{}}`)) {
		t.Fatal("empty usage was accepted")
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run:

```powershell
go test ./... -run "TestResetProbeConstantsAvoidEarlyMisclassification|TestLooksLikeLazyReset|TestMonthlyProbeRequiresWindowSeconds|TestResetProbeUsageEvidence"
```

Expected: FAIL with missing constants and helper functions.

- [ ] **Step 3: Add probe model fields**

In `models.go`, add:

```go
type ResetProbeStatus string

const (
	ResetProbeStatusNone            ResetProbeStatus = ""
	ResetProbeStatusPending         ResetProbeStatus = "pending"
	ResetProbeStatusConfirmedActive ResetProbeStatus = "confirmed_active"
	ResetProbeStatusVerified        ResetProbeStatus = "verified"
	ResetProbeStatusFailed          ResetProbeStatus = "failed"
)

type ResetProbeState struct {
	WindowKind    WindowKind
	WindowSeconds int64
	ResetAt       time.Time
	NextCheckAt   time.Time
	LastProbeAt   time.Time
	VerifiedAt    time.Time
	Status        ResetProbeStatus
	Error         string
}
```

Add to `AccountState`:

```go
ResetProbe ResetProbeState
```

- [ ] **Step 4: Create probe helpers**

Create `probe.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"time"
)

const (
	resetProbeAfterResetDelay = 10 * time.Minute
	resetProbeCloseThreshold  = 3 * time.Minute
	codexResetProbeEndpoint   = "https://chatgpt.com/backend-api/codex/responses/compact"
	codexResetProbeModel      = "gpt-5.4-mini"
)

var resetProbePayload = []byte(`{"model":"gpt-5.4-mini","instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)

func probeWindowDuration(window QuotaWindow) (time.Duration, bool) {
	if window.LimitWindowSeconds != nil && *window.LimitWindowSeconds > 0 {
		return time.Duration(*window.LimitWindowSeconds) * time.Second, true
	}
	switch window.Kind {
	case WindowFiveHour:
		return 5 * time.Hour, true
	case WindowWeekly:
		return 7 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

func looksLikeLazyReset(now time.Time, window QuotaWindow) bool {
	duration, ok := probeWindowDuration(window)
	if !ok || window.ResetAt.IsZero() {
		return false
	}
	return absDuration(window.ResetAt.Sub(now.Add(duration))) <= resetProbeCloseThreshold
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func resetProbeUsageEvidence(body []byte) bool {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return false
	}
	for _, path := range [][]string{
		{"usage", "total_tokens"},
		{"usage", "prompt_tokens"},
		{"usage", "input_tokens"},
		{"usage", "completion_tokens"},
		{"usage", "output_tokens"},
		{"response", "usage", "total_tokens"},
		{"response", "usage", "prompt_tokens"},
		{"response", "usage", "input_tokens"},
		{"response", "usage", "completion_tokens"},
		{"response", "usage", "output_tokens"},
	} {
		if jsonNumberPathPositive(doc, path...) {
			return true
		}
	}
	return false
}

func jsonNumberPathPositive(root any, path ...string) bool {
	current := root
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current = m[key]
	}
	switch v := current.(type) {
	case float64:
		return v > 0
	case int:
		return v > 0
	case int64:
		return v > 0
	case json.Number:
		i, err := v.Int64()
		return err == nil && i > 0
	default:
		return false
	}
}

func resetProbePayloadBytes() []byte {
	return bytes.Clone(resetProbePayload)
}
```

- [ ] **Step 5: Run tests**

Run:

```powershell
go test ./... -run "TestResetProbeConstantsAvoidEarlyMisclassification|TestLooksLikeLazyReset|TestMonthlyProbeRequiresWindowSeconds|TestResetProbeUsageEvidence"
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add models.go probe.go state_test.go
git commit -m "feat: add reset probe state helpers"
```

## Task 3: Pending Probe Scheduling

**Files:**
- Modify: `probe.go`
- Modify: `state.go`
- Modify: `state_test.go`

- [ ] **Step 1: Write failing state scheduling tests**

Add to `state_test.go`:

```go
func TestScheduleResetProbeFromMaturedPreviousReset(t *testing.T) {
	resetAt := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	now := resetAt.Add(time.Minute)
	seconds := int64(fiveHourSeconds)
	previous := AccountState{
		AuthID: "auth-1",
		Quota: ParsedQuota{
			FiveHour: &QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: resetAt},
		},
	}
	next, ok := scheduleResetProbeFromPrevious(previous, ResetProbeState{}, now)
	if !ok {
		t.Fatal("scheduleResetProbeFromPrevious ok=false, want true")
	}
	if next.Status != ResetProbeStatusPending {
		t.Fatalf("Status = %q, want pending", next.Status)
	}
	if !next.ResetAt.Equal(resetAt) {
		t.Fatalf("ResetAt = %s, want %s", next.ResetAt, resetAt)
	}
	if !next.NextCheckAt.Equal(resetAt.Add(10 * time.Minute)) {
		t.Fatalf("NextCheckAt = %s, want %s", next.NextCheckAt, resetAt.Add(10*time.Minute))
	}
}

func TestNextRefreshDueAtIncludesPendingResetProbe(t *testing.T) {
	resetAt := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	now := resetAt.Add(2 * time.Minute)
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{
		AuthID: "auth-1",
		ResetProbe: ResetProbeState{
			Status:      ResetProbeStatusPending,
			ResetAt:     resetAt,
			NextCheckAt: resetAt.Add(10 * time.Minute),
		},
		LastSuccessAt: now,
	})
	next := store.NextRefreshDueAt(now)
	if !next.Equal(resetAt.Add(10 * time.Minute)) {
		t.Fatalf("NextRefreshDueAt = %s, want %s", next, resetAt.Add(10*time.Minute))
	}
}
```

- [ ] **Step 2: Run tests to verify failure**

Run:

```powershell
go test ./... -run "TestScheduleResetProbeFromMaturedPreviousReset|TestNextRefreshDueAtIncludesPendingResetProbe"
```

Expected: FAIL with missing scheduler helper or missing due-time integration.

- [ ] **Step 3: Implement pending scheduler helper**

Add to `probe.go`:

```go
func scheduleResetProbeFromPrevious(previous AccountState, current ResetProbeState, now time.Time) (ResetProbeState, bool) {
	if current.Status == ResetProbeStatusPending && !current.NextCheckAt.IsZero() && current.NextCheckAt.After(now) {
		return current, true
	}
	for _, window := range resetProbeCandidateWindows(previous.Quota) {
		duration, ok := probeWindowDuration(window)
		if !ok || duration <= 0 || window.ResetAt.IsZero() {
			continue
		}
		if window.ResetAt.After(now) {
			continue
		}
		seconds := int64(duration / time.Second)
		return ResetProbeState{
			WindowKind:    window.Kind,
			WindowSeconds: seconds,
			ResetAt:       window.ResetAt,
			NextCheckAt:   window.ResetAt.Add(resetProbeAfterResetDelay),
			Status:        ResetProbeStatusPending,
		}, true
	}
	return current, current.Status != ResetProbeStatusNone
}

func resetProbeCandidateWindows(quota ParsedQuota) []QuotaWindow {
	windows := make([]QuotaWindow, 0, 2)
	if quota.FiveHour != nil {
		windows = append(windows, *quota.FiveHour)
	}
	if quota.LongWindow != nil {
		windows = append(windows, *quota.LongWindow)
	}
	return windows
}

func resetProbeDue(probe ResetProbeState, now time.Time) bool {
	return probe.Status == ResetProbeStatusPending && !probe.NextCheckAt.IsZero() && !probe.NextCheckAt.After(now)
}
```

- [ ] **Step 4: Integrate due-time calculation**

In `state.go`, inside `NextRefreshDueAt`, before stale/reset quota windows:

```go
if cfg.EnableResetProbe && account.ResetProbe.Status == ResetProbeStatusPending && !account.ResetProbe.NextCheckAt.IsZero() {
	consider(account.ResetProbe.NextCheckAt)
	continue
}
```

In `accountRefreshDue`, before ordinary reset due checks:

```go
if cfg.EnableResetProbe && resetProbeDue(account.ResetProbe, now) {
	return true, "reset_probe_check_due"
}
```

In `cloneAccountState`, no deep pointer clone is needed for `ResetProbeState`
because it contains value fields only.

- [ ] **Step 5: Run tests**

Run:

```powershell
go test ./... -run "TestScheduleResetProbeFromMaturedPreviousReset|TestNextRefreshDueAtIncludesPendingResetProbe"
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add probe.go state.go state_test.go
git commit -m "feat: schedule pending reset probes"
```

## Task 4: Probe Execution

**Files:**
- Modify: `probe.go`
- Modify: `refresh.go`
- Modify: `refresh_test.go`

- [ ] **Step 1: Write failing lazy-reset probe test**

Add to `refresh_test.go`:

```go
func TestRefreshDueRunsResetProbeForLazyWindow(t *testing.T) {
	resetAt := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	now := resetAt.Add(10 * time.Minute)
	seconds := int64(fiveHourSeconds)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		responseByURL: map[string]pluginapi.HTTPResponse{
			chatGPTQuotaEndpoint: {
				StatusCode: http.StatusOK,
				Headers:    http.Header{},
				Body:       []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_after_seconds":18000}}}`),
			},
			resetCreditsEndpoint: {
				StatusCode: http.StatusOK,
				Headers:    http.Header{},
				Body:       []byte(`{"available_count":0}`),
			},
			codexResetProbeEndpoint: {
				StatusCode: http.StatusOK,
				Headers:    http.Header{},
				Body:       []byte(`{"id":"probe","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`),
			},
		},
	}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{
		AuthID:    "auth-1",
		AuthIndex: "idx-1",
		Provider:  "codex",
		Quota: ParsedQuota{
			FiveHour: &QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: resetAt},
		},
		ResetProbe: ResetProbeState{
			WindowKind:    WindowFiveHour,
			WindowSeconds: fiveHourSeconds,
			ResetAt:       resetAt,
			NextCheckAt:   resetAt.Add(10 * time.Minute),
			Status:        ResetProbeStatusPending,
		},
		LastSuccessAt: resetAt.Add(-time.Hour),
	})

	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce returned error: %v", err)
	}
	urls := strings.Join(host.requestedURLs(), "\n")
	if !strings.Contains(urls, codexResetProbeEndpoint) {
		t.Fatalf("probe endpoint was not requested; urls:\n%s", urls)
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.ResetProbe.Status != ResetProbeStatusVerified {
		t.Fatalf("probe status = %q, want verified; error=%q", account.ResetProbe.Status, account.ResetProbe.Error)
	}
	if account.ResetProbe.VerifiedAt.IsZero() {
		t.Fatal("VerifiedAt is zero, want set")
	}
}
```

- [ ] **Step 2: Write non-lazy active-window test**

Add to `refresh_test.go`:

```go
func TestRefreshDueDoesNotProbeActiveWindow(t *testing.T) {
	resetAt := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	now := resetAt.Add(10 * time.Minute)
	seconds := int64(fiveHourSeconds)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		responseByURL: map[string]pluginapi.HTTPResponse{
			chatGPTQuotaEndpoint: {
				StatusCode: http.StatusOK,
				Headers:    http.Header{},
				Body:       []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_after_seconds":17400}}}`),
			},
			resetCreditsEndpoint: {
				StatusCode: http.StatusOK,
				Headers:    http.Header{},
				Body:       []byte(`{"available_count":0}`),
			},
		},
	}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{
		AuthID:    "auth-1",
		AuthIndex: "idx-1",
		Provider:  "codex",
		Quota: ParsedQuota{
			FiveHour: &QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: resetAt},
		},
		ResetProbe: ResetProbeState{
			WindowKind:    WindowFiveHour,
			WindowSeconds: fiveHourSeconds,
			ResetAt:       resetAt,
			NextCheckAt:   resetAt.Add(10 * time.Minute),
			Status:        ResetProbeStatusPending,
		},
		LastSuccessAt: resetAt.Add(-time.Hour),
	})

	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce returned error: %v", err)
	}
	urls := strings.Join(host.requestedURLs(), "\n")
	if strings.Contains(urls, codexResetProbeEndpoint) {
		t.Fatalf("probe endpoint was requested for active window; urls:\n%s", urls)
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.ResetProbe.Status != ResetProbeStatusConfirmedActive {
		t.Fatalf("probe status = %q, want confirmed_active", account.ResetProbe.Status)
	}
}
```

- [ ] **Step 3: Run tests to verify failure**

Run:

```powershell
go test ./... -run "TestRefreshDueRunsResetProbeForLazyWindow|TestRefreshDueDoesNotProbeActiveWindow"
```

Expected: FAIL because refresh does not execute probe state yet.

- [ ] **Step 4: Add matching window and transition helpers**

Add to `probe.go`:

```go
func matchingProbeWindow(quota ParsedQuota, probe ResetProbeState) (QuotaWindow, bool) {
	for _, window := range resetProbeCandidateWindows(quota) {
		if probe.WindowKind != "" && window.Kind != probe.WindowKind {
			continue
		}
		return window, true
	}
	return QuotaWindow{}, false
}

func markResetProbeConfirmedActive(probe ResetProbeState) ResetProbeState {
	probe.Status = ResetProbeStatusConfirmedActive
	probe.Error = ""
	return probe
}

func markResetProbeVerified(probe ResetProbeState, now time.Time) ResetProbeState {
	probe.Status = ResetProbeStatusVerified
	probe.LastProbeAt = now
	probe.VerifiedAt = now
	probe.Error = ""
	return probe
}

func markResetProbeFailed(probe ResetProbeState, now time.Time, err error) ResetProbeState {
	probe.Status = ResetProbeStatusFailed
	probe.LastProbeAt = now
	probe.Error = ""
	if err != nil {
		probe.Error = redactSecrets(err.Error())
	}
	return probe
}
```

- [ ] **Step 5: Add probe HTTP method**

In `refresh.go`, add:

```go
func (r *QuotaRefresher) runResetProbe(credentials CodexCredentials) error {
	req := pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    codexResetProbeEndpoint,
		Headers: http.Header{
			"Authorization":      []string{"Bearer " + credentials.AccessToken},
			"Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID},
			"Accept":             []string{"application/json"},
			"Content-Type":       []string{"application/json"},
			"User-Agent":         []string{quotaUserAgent},
		},
		Body: resetProbePayloadBytes(),
	}
	resp, err := r.host.Do(req)
	if err != nil {
		return fmt.Errorf("reset probe request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("reset probe returned status %d: response body: %s", resp.StatusCode, sanitizedBodySummary(resp.Body))
	}
	if !resetProbeUsageEvidence(resp.Body) {
		return fmt.Errorf("reset probe response did not include usage evidence")
	}
	return nil
}
```

- [ ] **Step 6: Add post-probe quota fetch helper**

Extract the existing quota GET logic in `refreshAuth` into:

```go
func (r *QuotaRefresher) fetchQuota(credentials CodexCredentials) (ParsedQuota, error) {
	cfg := r.state.Config()
	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    cfg.QuotaEndpoint,
		Headers: http.Header{
			"Authorization":      []string{"Bearer " + credentials.AccessToken},
			"Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID},
			"Content-Type":       []string{"application/json"},
			"User-Agent":         []string{quotaUserAgent},
		},
	}
	resp, err := r.host.Do(req)
	if err != nil {
		return ParsedQuota{}, fmt.Errorf("quota request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ParsedQuota{}, quotaStatusError{status: resp.StatusCode, body: resp.Body}
	}
	quota, err := ParseCodexUsagePayload(resp.Body, r.now())
	if err != nil {
		return ParsedQuota{}, fmt.Errorf("parse quota: %w", err)
	}
	return quota, nil
}

type quotaStatusError struct {
	status int
	body   []byte
}

func (e quotaStatusError) Error() string {
	return fmt.Sprintf("quota request returned status %d: response body: %s", e.status, sanitizedBodySummary(e.body))
}
```

Update `refreshAuth` to call `fetchQuota` and keep the existing failure
classification by checking `errors.As(err, &statusErr)`.

- [ ] **Step 7: Execute due probe inside refresh**

In `refreshAuth`, after the first quota parse and reset-credit merge, before
`state.UpsertQuota(account)`, add:

```go
previous := r.mergeExistingAccount(account)
if r.state.Config().EnableResetProbe {
	probe, keep := scheduleResetProbeFromPrevious(previous, previous.ResetProbe, r.now())
	if keep {
		account.ResetProbe = probe
	}
	if resetProbeDue(account.ResetProbe, r.now()) {
		if window, ok := matchingProbeWindow(quota, account.ResetProbe); ok && looksLikeLazyReset(r.now(), window) {
			if err := r.runResetProbe(credentials); err != nil {
				account.ResetProbe = markResetProbeFailed(account.ResetProbe, r.now(), err)
				r.state.RecordLog("warn", "quota.reset_probe_failed", "Codex reset probe failed", map[string]any{"auth_id": account.AuthID, "error": account.ResetProbe.Error}, r.now())
			} else {
				account.ResetProbe = markResetProbeVerified(account.ResetProbe, r.now())
				r.state.RecordLog("info", "quota.reset_probe_verified", "Codex reset probe verified", map[string]any{"auth_id": account.AuthID}, r.now())
				if refreshedQuota, err := r.fetchQuota(credentials); err == nil {
					quota = refreshedQuota
					if resetCredits, err := r.refreshResetCredits(credentials); err == nil {
						mergeResetCredits(&quota, resetCredits)
					}
				}
			}
		} else {
			account.ResetProbe = markResetProbeConfirmedActive(account.ResetProbe)
		}
	}
}
```

Ensure `account.Quota = quota` is assigned after this block.

- [ ] **Step 8: Run tests**

Run:

```powershell
go test ./... -run "TestRefreshDueRunsResetProbeForLazyWindow|TestRefreshDueDoesNotProbeActiveWindow|TestRefreshOnceHTTPNon2xxRecordsRedactedErrorWithoutQuotaSuccess"
```

Expected: PASS.

- [ ] **Step 9: Commit**

```powershell
git add probe.go refresh.go refresh_test.go
git commit -m "feat: execute lazy reset probes"
```

## Task 5: Failure And Loop Prevention

**Files:**
- Modify: `refresh_test.go`
- Modify: `state_test.go`
- Modify: `probe.go`
- Modify: `state.go`

- [ ] **Step 1: Write failed-probe test**

Add to `refresh_test.go`:

```go
func TestRefreshDueFailedResetProbeDoesNotTightLoop(t *testing.T) {
	resetAt := time.Date(2026, 6, 30, 10, 0, 0, 0, time.UTC)
	now := resetAt.Add(10 * time.Minute)
	seconds := int64(fiveHourSeconds)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		responseByURL: map[string]pluginapi.HTTPResponse{
			chatGPTQuotaEndpoint:  {StatusCode: http.StatusOK, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_after_seconds":18000}}}`)},
			resetCreditsEndpoint:  {StatusCode: http.StatusOK, Body: []byte(`{"available_count":0}`)},
			codexResetProbeEndpoint: {StatusCode: http.StatusOK, Body: []byte(`{"id":"probe","usage":{}}`)},
		},
	}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{
		AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		Quota: ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: resetAt}},
		ResetProbe: ResetProbeState{WindowKind: WindowFiveHour, WindowSeconds: fiveHourSeconds, ResetAt: resetAt, NextCheckAt: now, Status: ResetProbeStatusPending},
		LastSuccessAt: resetAt.Add(-time.Hour),
	})
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshDueOnce(); err != nil {
		t.Fatalf("RefreshDueOnce returned error: %v", err)
	}
	account := accountByAuthID(t, store.Snapshot(now), "auth-1")
	if account.ResetProbe.Status != ResetProbeStatusFailed {
		t.Fatalf("Status = %q, want failed", account.ResetProbe.Status)
	}
	if account.ResetProbe.Error == "" {
		t.Fatal("Error is empty, want sanitized failure")
	}
	if due, reason := accountRefreshDue(account, cfg, now.Add(time.Second)); due && reason == "reset_probe_check_due" {
		t.Fatal("failed probe remained immediately due, want no tight loop")
	}
}
```

- [ ] **Step 2: Run test**

Run:

```powershell
go test ./... -run TestRefreshDueFailedResetProbeDoesNotTightLoop
```

Expected: PASS if failed probes are not considered pending; otherwise FAIL.

- [ ] **Step 3: Ensure due logic only accepts pending**

In `probe.go`, keep `resetProbeDue` limited to:

```go
return probe.Status == ResetProbeStatusPending && !probe.NextCheckAt.IsZero() && !probe.NextCheckAt.After(now)
```

In `scheduleResetProbeFromPrevious`, do not recreate a pending probe when
`current.ResetAt.Equal(window.ResetAt)` and `current.Status` is `failed`,
`verified`, or `confirmed_active`:

```go
if !current.ResetAt.IsZero() && current.ResetAt.Equal(window.ResetAt) && current.Status != ResetProbeStatusPending {
	return current, true
}
```

- [ ] **Step 4: Run tests**

Run:

```powershell
go test ./... -run "TestRefreshDueFailedResetProbeDoesNotTightLoop|TestScheduleResetProbeFromMaturedPreviousReset"
```

Expected: PASS.

- [ ] **Step 5: Commit**

```powershell
git add probe.go state.go refresh_test.go state_test.go
git commit -m "fix: prevent reset probe tight loops"
```

## Task 6: Management UI

**Files:**
- Modify: `management.go`
- Modify: `management_test.go`

- [ ] **Step 1: Write failing UI tests**

Add to `management_test.go`:

```go
func TestStatusPageShowsResetProbeWarningOutsideSettingsPanel(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	page := renderStatusPageForTest(t, store)
	warningIndex := strings.Index(page, `resetProbeWarning`)
	settingsIndex := strings.Index(page, `id="settingsPanel"`)
	if warningIndex < 0 {
		t.Fatal("resetProbeWarning not rendered")
	}
	if settingsIndex < 0 {
		t.Fatal("settingsPanel not rendered")
	}
	if warningIndex > settingsIndex {
		t.Fatalf("resetProbeWarning appears after settingsPanel; warning must be visible before collapsed settings")
	}
	if !strings.Contains(page, `name="enable_reset_probe"`) {
		t.Fatal("enable_reset_probe checkbox not rendered")
	}
}
```

If `renderStatusPageForTest` is not present, add the same helper used by
existing management rendering tests.

- [ ] **Step 2: Run test to verify failure**

Run:

```powershell
go test ./... -run TestStatusPageShowsResetProbeWarningOutsideSettingsPanel
```

Expected: FAIL until UI markup is added.

- [ ] **Step 3: Add visible warning markup**

In `statusTemplateV2`, before the settings `<details id="settingsPanel">`, add:

```html
<div class="notice warn" id="resetProbeWarning">
  <strong data-i18n="resetProbe.warningTitle">Automatic reset probe is off by default.</strong>
  <span data-i18n="resetProbe.warningBody">Enable it only if you want the plugin to send a tiny Codex request after a reset appears lazy, so the next quota window starts counting down.</span>
</div>
```

- [ ] **Step 4: Add checkbox inside scheduling settings**

Inside the settings form, add:

```html
<label class="checkline">
  <input type="checkbox" name="enable_reset_probe" id="enableResetProbe">
  <span data-i18n="settings.enableResetProbe">Enable automatic reset probe</span>
</label>
```

In `fillSettings()`:

```js
setChecked('enableResetProbe', SETTINGS.enable_reset_probe);
```

In `collectSettingsPayload()`:

```js
enable_reset_probe: byId('enableResetProbe').checked,
```

Add translations for `resetProbe.warningTitle`, `resetProbe.warningBody`, and
`settings.enableResetProbe` in English and Chinese.

- [ ] **Step 5: Add probe fields to account status JSON**

In the account status struct in `management.go`, add:

```go
ResetProbeStatus      string `json:"reset_probe_status,omitempty"`
ResetProbeNextCheck   string `json:"reset_probe_next_check,omitempty"`
ResetProbeLastProbe   string `json:"reset_probe_last_probe,omitempty"`
ResetProbeVerifiedAt  string `json:"reset_probe_verified_at,omitempty"`
ResetProbeError       string `json:"reset_probe_error,omitempty"`
```

Populate from `AccountState.ResetProbe` with existing `formatTime` helper.

- [ ] **Step 6: Run tests**

Run:

```powershell
go test ./... -run "TestStatusPageShowsResetProbeWarningOutsideSettingsPanel|TestSettingsPayloadIncludesResetProbeFlag"
```

Expected: PASS.

- [ ] **Step 7: Commit**

```powershell
git add management.go management_test.go
git commit -m "feat: expose reset probe controls"
```

## Task 7: Full Verification

**Files:**
- No new files unless a previous task revealed a required fixture.

- [ ] **Step 1: Run full test suite**

Run:

```powershell
go test ./...
```

Expected: PASS.

- [ ] **Step 2: Verify probe constants and endpoint references**

Run:

```powershell
rg -n "resetProbeAfterResetDelay|resetProbeCloseThreshold|codexResetProbeEndpoint|enable_reset_probe" .
```

Expected:

- `resetProbeAfterResetDelay` is defined once as `10 * time.Minute`;
- `resetProbeCloseThreshold` is defined once as `3 * time.Minute`;
- `codexResetProbeEndpoint` is a fixed constant;
- `enable_reset_probe` appears in config, settings, UI, tests, and docs.

- [ ] **Step 3: Verify no host.model probe path was introduced**

Run:

```powershell
rg -n "host\\.model|MethodHostModel|PinnedAuth|pinned_auth" .
```

Expected: no reset-probe implementation depends on `host.model.execute`,
`PinnedAuth`, or `pinned_auth_id`.

- [ ] **Step 4: Build plugin**

Run:

```powershell
go build -buildmode=c-shared -o dist/codex-quota-scheduler.dll .
```

Expected: command exits 0 and writes `dist/codex-quota-scheduler.dll`.

- [ ] **Step 5: Check worktree**

Run:

```powershell
git status --short
git log --oneline -8
```

Expected: clean working tree with the task commits on the reset-probe branch.

## Self-Review

- Spec coverage: config, fixed internal timing, cache-driven scheduling,
  lazy-reset detection, per-auth `host.http.do` probe, usage evidence,
  post-probe quota refresh, monthly duration handling, UI warning, security, and
  loop prevention are covered.
- Placeholder scan: the plan contains no placeholder or deferred implementation
  markers.
- Type consistency: the plan consistently uses `EnableResetProbe`,
  `ResetProbeState`, `ResetProbeStatusPending`,
  `resetProbeAfterResetDelay`, `resetProbeCloseThreshold`,
  `codexResetProbeEndpoint`, `looksLikeLazyReset`, and
  `resetProbeUsageEvidence`.
