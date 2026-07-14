# Quota Window and Roster Admission Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep Codex accounts schedulable when OpenAI omits FiveHour but returns a valid LongWindow, and make successful authoritative roster publication authorize real Management/background quota refreshes.

**Architecture:** Treat observed quota windows independently: LongWindow remains mandatory, FiveHour is validated only when present, and a focused Probe reconciliation helper removes obsolete per-window state while preserving nonterminal durable attempts. At the roster boundary, derive one complete `CPAAdmissionState` from the authoritative highest tier and publish it only after bindings, persistence, removal fencing, and Probe recovery succeed.

**Tech Stack:** Go 1.26, CGO Windows `-buildmode=c-shared`, CLIProxyAPI plugin ABI, standard-library JSON/HTTP/synchronization, Go `testing`, PowerShell gate/build scripts.

## Global Constraints

- Do not edit `docs/fable-spec/refactor-decision-spec.md`; DEV-010 and DEV-011 record the approved post-freeze decisions.
- FiveHour is optional only when a recognized Weekly or Monthly LongWindow has a non-zero reset.
- Never synthesize a FiveHour window, reset, usage value, or Probe baseline.
- Preserve a missing window while a nonterminal durable Probe attempt references it.
- Runtime admission comes only from the authoritative highest Codex tier, never scheduler candidates.
- Do not authorize the new roster until binding reconciliation, persistence, removal fencing, and Probe recovery all succeed.
- Preserve all untracked `.superpowers/sdd/*` workflow artifacts.

---

### Task 1: Make FiveHour optional and reconcile missing Probe windows

**Files:**
- Modify: `quota_test.go`
- Modify: `scheduler_test.go`
- Modify: `scheduler.go:221-255`
- Modify: `probe_state.go`
- Modify: `probe_runtime.go`
- Modify: `probe_runtime_test.go`
- Modify: `management_test.go`

**Interfaces:**
- Consumes: `ParseCodexUsagePayload([]byte, time.Time) (ParsedQuota, error)`, `accountQueueState(AccountState, time.Time) (QueueStatus, bool, string, time.Time)`, `ProbeAttempt`, `ProbeController`, `StateStore`
- Produces: `(*ProbeController).RemoveWindow(AuthInstanceID, ProbeWindowKind)`, `(*QuotaRefresher).reconcileObservedProbeWindows(AuthInstanceID, ParsedQuota) error`

- [ ] **Step 1: Add parser and scheduler regression tests**

Add these focused tests:

```go
func TestParseCodexUsagePayloadAllowsSecondaryOnlyLongWindow(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	parsed, err := ParseCodexUsagePayload([]byte(`{"rate_limit":{"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.FiveHour != nil || parsed.LongWindow == nil || parsed.LongWindow.Kind != WindowWeekly || parsed.Family != AccountFamilyWeekly {
		t.Fatalf("parsed = %#v, want secondary-only weekly quota", parsed)
	}
}

func TestPickAllowsWeeklyWithoutFiveHour(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	account := weeklyAccount("weekly", 0, now.Add(24*time.Hour), false)
	account.Quota.FiveHour = nil
	status, available, reason, sortAt := accountQueueState(account, now)
	if status != QueueStatusAvailable || !available || reason != "" || !sortAt.Equal(account.Quota.LongWindow.ResetAt) {
		t.Fatalf("state = %s %v %q %s", status, available, reason, sortAt)
	}
}

func TestPickRejectsAccountWithoutLongWindow(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	account := weeklyAccount("unknown", 0, now.Add(24*time.Hour), false)
	account.Quota.FiveHour = nil
	account.Quota.LongWindow = nil
	status, available, _, _ := accountQueueState(account, now)
	if status != QueueStatusUnavailable || available {
		t.Fatalf("state = %s %v, want unavailable", status, available)
	}
}
```

Add the Monthly counterpart and retain explicit existing tests for FiveHour exhaustion and zero reset.

- [ ] **Step 2: Run parser/scheduler tests and verify RED**

Run:

```powershell
go test . -run 'TestParseCodexUsagePayloadAllowsSecondaryOnlyLongWindow|TestPickAllows(Weekly|Monthly)WithoutFiveHour|TestPickRejectsAccountWithoutLongWindow' -count=1
```

Expected: the parser test passes as characterization; Weekly scheduling fails with `missing_five_hour_window`. The combined command must exit non-zero because the behavior regression is RED.

- [ ] **Step 3: Implement minimal scheduler validation**

In both Weekly and Monthly branches, validate LongWindow first and guard FiveHour-specific checks:

```go
if account.Quota.LongWindow == nil {
	return QueueStatusUnavailable, false, missingLongReason, time.Time{}
}
if account.Quota.LongWindow.ResetAt.IsZero() {
	return QueueStatusUnavailable, false, missingLongResetReason, time.Time{}
}
if windowExhausted(account.Quota.LongWindow, now) {
	return QueueStatusLongWindowExhausted, false, exhaustedLongReason, account.Quota.LongWindow.ResetAt
}
if account.Quota.FiveHour != nil {
	if account.Quota.FiveHour.ResetAt.IsZero() {
		return QueueStatusUnavailable, false, "missing_five_hour_reset", time.Time{}
	}
	if windowExhausted(account.Quota.FiveHour, now) {
		return QueueStatusFiveHourExhausted, false, "five_hour_exhausted", account.Quota.FiveHour.ResetAt
	}
}
return QueueStatusAvailable, true, "", account.Quota.LongWindow.ResetAt
```

Use the existing Weekly/Monthly reason strings; do not introduce generic variables in production solely to match the plan snippet.

- [ ] **Step 4: Run scheduler tests and verify GREEN**

Run the Step 2 command again.

Expected: PASS. Also run:

```powershell
go test . -run 'TestPickSkips(Weekly|Monthly)When(FiveHour|LongWindow)|TestPickAllowsWeeklyWhenExhaustedFiveHourResetPassed' -count=1
```

Expected: PASS.

- [ ] **Step 5: Add failing Probe cleanup tests**

Add tests that create an active binding with persisted/controller FiveHour and Long windows, then call the desired reconciliation helper:

```go
func TestProbeRefreshRemovesAbsentFiveHourWindow(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, &sequenceProbeHost{})
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeWaitingReset})
	r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeWaitingReset})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	quota := ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: r.now().Add(24 * time.Hour)}}
	if err := r.reconcileObservedProbeWindows(binding.Instance, quota); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour); ok {
		t.Fatal("absent FiveHour Probe window retained")
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowLong); !ok {
		t.Fatal("LongWindow Probe state removed")
	}
}

func TestProbeRefreshPreservesAbsentFiveHourDuringNonterminalAttempt(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, &sequenceProbeHost{})
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeSentUnknown})
	if _, err := r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeWindows = r.probeController.Snapshot()
		s.ProbeAttempts[binding.Instance] = ProbeAttempt{Instance: binding.Instance, AttemptID: "active", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	quota := ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: r.now().Add(24 * time.Hour)}}
	if err := r.reconcileObservedProbeWindows(binding.Instance, quota); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour); !ok {
		t.Fatal("nonterminal attempt lost referenced FiveHour state")
	}
}
```

Use `newDueProbeRuntime` and add table rows for `ProbeAttemptPrepared`,
`ProbeAttemptSending`, `ProbeAttemptSent`, and `ProbeAttemptSentUnknown`. Add a
no-attempt row that must remove FiveHour and a reappearance test that calls
`bootstrapProbeWindows` after publishing quota with FiveHour and must recreate
`ProbeWindowFiveHour`.

- [ ] **Step 6: Run Probe cleanup tests and verify RED**

Run:

```powershell
go test . -run 'TestProbeRefresh(RemovesAbsentFiveHourWindow|PreservesAbsentFiveHourDuringNonterminalAttempt)|TestProbeBootstrapRecreatesReappearedFiveHour' -count=1
```

Expected: compile failure because `reconcileObservedProbeWindows` and `RemoveWindow` do not exist.

- [ ] **Step 7: Implement per-window reconciliation**

Add a controller method that deletes only one window and removes the instance map only when empty:

```go
func (c *ProbeController) RemoveWindow(instance AuthInstanceID, kind ProbeWindowKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.windows[instance], kind)
	if len(c.windows[instance]) == 0 {
		delete(c.windows, instance)
	}
}
```

Add helpers in `probe_runtime.go`:

```go
func nonterminalProbeAttempt(attempt ProbeAttempt) bool {
	switch attempt.Phase {
	case ProbeAttemptPrepared, ProbeAttemptSending, ProbeAttemptSent, ProbeAttemptSentUnknown:
		return true
	default:
		return false
	}
}

func attemptReferencesWindow(attempt ProbeAttempt, kind ProbeWindowKind) bool {
	for _, candidate := range attempt.Windows {
		if candidate == kind {
			return true
		}
	}
	return false
}

func (r *QuotaRefresher) reconcileObservedProbeWindows(instance AuthInstanceID, quota ParsedQuota) error {
	if r.runtimeStore == nil || r.probeController == nil || instance == 0 {
		return nil
	}
	observed := map[ProbeWindowKind]bool{ProbeWindowFiveHour: quota.FiveHour != nil, ProbeWindowLong: quota.LongWindow != nil}
	updated, err := r.runtimeStore.Update(func(persisted *PersistentState) error {
		attempt := persisted.ProbeAttempts[instance]
		for kind, present := range observed {
			if present || nonterminalProbeAttempt(attempt) && attemptReferencesWindow(attempt, kind) {
				continue
			}
			delete(persisted.ProbeWindows[instance], kind)
			if len(persisted.ProbeWindows[instance]) == 0 {
				delete(persisted.ProbeWindows, instance)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for kind, present := range observed {
		if !present {
			if _, retained := updated.ProbeWindows[instance][kind]; !retained {
				r.probeController.RemoveWindow(instance, kind)
			}
		}
	}
	return nil
}
```

Wire the helper only after a successful parsed quota response, using the active binding's `Instance`. Run it before publishing quota success; on reconciliation error, follow the existing refresh-failure/log path instead of claiming full success. In typed Probe verify/precheck paths, call reconciliation after the durable attempt becomes terminal so a missing window can be deleted safely.

After normal quota success is applied to `PluginState`, call
`bootstrapProbeWindows` so a newly reappeared FiveHour window is created from
the observed quota. If bootstrap persistence fails after the quota writeback,
record a warning and let the next refresh retry Probe bootstrap; do not roll
back authoritative quota data or synthesize state from another source.

- [ ] **Step 8: Run Probe tests and verify GREEN**

Run the Step 6 command, then:

```powershell
go test . -run 'TestProbe|TestTypedProbe|TestManagement.*FiveHour' -count=1 -timeout 180s
```

Expected: PASS.

- [ ] **Step 9: Lock Management hidden-row behavior**

Add a payload/template regression that creates an available Weekly account with nil FiveHour and a valid LongWindow. Assert `five_hour.missing == true`, the account is available, and both server-rendered and dynamic paths contain the existing missing guard. Do not change production UI unless the test exposes a regression.

- [ ] **Step 10: Commit Task 1**

```powershell
git add -- quota_test.go scheduler.go scheduler_test.go probe_state.go probe_runtime.go probe_runtime_test.go management_test.go
git diff --cached --check
git commit -m "fix: allow Codex quota without FiveHour"
```

---

### Task 2: Publish authoritative roster runtime admission

**Files:**
- Modify: `refresh.go:540-648`
- Modify: `runtime_wiring_test.go`
- Modify: `refresh_test.go`

**Interfaces:**
- Consumes: `HighestCodexTier([]RosterEntry) (int, []string, bool)`, `(*PluginState).ReplaceCPAAdmission(CPAAdmissionState) uint64`, existing admission permit/version checks
- Produces: authoritative roster publication that leaves `PluginState.CPAAdmission()` equal to the active highest tier

- [ ] **Step 1: Add a production-wiring manual-refresh regression**

Create a production host fixture that returns two highest-tier Codex auths and one lower-tier auth, valid credentials, and a secondary-only LongWindow quota response. The test must not call `ReplaceCPAAdmission` directly:

```go
func TestProductionRosterPublicationEnablesManualRefresh(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := &countingProductionHost{
		httpResp: pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"rate_limit":{"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`)},
		auth: map[string]pluginapi.HostAuthGetResponse{
			"a": {AuthIndex: "a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"a","refresh_token":"ra","account_id":"acct-a"}`)},
			"b": {AuthIndex: "b", Name: "b.json", JSON: json.RawMessage(`{"access_token":"b","refresh_token":"rb","account_id":"acct-b"}`)},
			"low": {AuthIndex: "low", Name: "low.json", JSON: json.RawMessage(`{"access_token":"low","refresh_token":"rl","account_id":"acct-low"}`)},
		},
	}
	state := NewPluginState(DefaultConfig())
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Entries: []RosterEntry{
		{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(9)},
		{ID: "b", AuthIndex: "b", Provider: "codex", Priority: intPtr(9)},
		{ID: "low", AuthIndex: "low", Provider: "codex", Priority: intPtr(1)},
	}}
	if err := r.PublishAuthoritativeRoster(context.Background(), roster); err != nil {
		t.Fatal(err)
	}
	admission, _ := r.state.CPAAdmissionVersioned()
	if !admission.Observed || admission.Priority != 9 || len(admission.AuthIDs) != 2 {
		t.Fatalf("admission = %#v", admission)
	}
	getBefore, httpBefore := host.get, host.http
	if err := r.RefreshOnce(); err != nil {
		t.Fatal(err)
	}
	if host.get-getBefore != 2 || host.http-httpBefore < 2 {
		t.Fatalf("refresh deltas get=%d http=%d", host.get-getBefore, host.http-httpBefore)
	}
	if snapshot := r.state.Snapshot(now); snapshot.LastAuthScanAt.IsZero() || snapshot.CodexAuthCount != 2 || len(snapshot.Accounts) != 2 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}
```

The production runtime intentionally sources the logical auth scan from its
confirmed roster rather than calling the host's mutable `ListAuths` again.
Assert the scan evidence plus actual `GetAuth` and quota HTTP call deltas;
assert `host.list == 0` to preserve roster authority.

- [ ] **Step 2: Run production-wiring test and verify RED**

Run:

```powershell
go test . -run 'TestProductionRosterPublicationEnablesManualRefresh' -count=1
```

Expected: FAIL because admission remains `Observed:false` and refresh performs no auth or HTTP calls.

- [ ] **Step 3: Add replacement/fencing regression**

Extend the existing roster replacement test or add `TestProductionRosterReplacementReplacesAdmissionAndFencesStaleRefresh`. Publish tier 1 `{a,b}`, capture the admission version, then publish tier 9 `{c}` and assert:

```go
next, nextVersion := r.state.CPAAdmissionVersioned()
if nextVersion <= oldVersion || next.Priority != 9 || len(next.AuthIDs) != 1 {
	t.Fatalf("replacement admission=%#v version=%d old=%d", next, nextVersion, oldVersion)
}
if _, ok := next.AuthIDs["c"]; !ok || r.state.BeginCPAAdmissionVersionCall(oldVersion) {
	t.Fatal("stale roster admission remained current")
}
```

Also assert removed account state is pruned and reuse an existing blocking-host pattern to prove an in-flight old-version result cannot publish after replacement.

- [ ] **Step 4: Run replacement test and verify RED**

Run:

```powershell
go test . -run 'TestProductionRosterReplacement(ReplacesAdmissionAndFencesStaleRefresh|PersistsFencesBeforePublication)' -count=1
```

Expected: the new admission assertions fail under current production code.

- [ ] **Step 5: Publish admission at the final authority boundary**

Capture the priority returned by `HighestCodexTier` and, after successful Probe recovery but before publishing scheduler/runtime roster state, call:

```go
priority, ids, ok := HighestCodexTier(roster.Entries)
// existing Capability-A and ok checks

admission := CPAAdmissionState{
	Observed: true,
	Priority: priority,
	AuthIDs:  make(map[string]struct{}, len(ids)),
}
for _, id := range ids {
	admission.AuthIDs[id] = struct{}{}
}

// after reconciliation, persistence, cancellation, and Probe recovery
r.state.ReplaceCPAAdmission(admission)
publishSchedulerState(r.state, allowed, r.now())
```

Reuse one ID set for filtering/snapshot publication only when doing so cannot alias mutable state. Do not publish admission on any error path or derive it from `allowed` after partial mutation.

- [ ] **Step 6: Run focused admission tests and verify GREEN**

Run Steps 2 and 4 again, then:

```powershell
go test . -run 'TestProductionRoster|TestRosterPublication|TestReplaceCPAAdmission|TestRefresh.*Admission' -count=1 -timeout 180s
```

Expected: PASS, including existing fail-closed genesis and stale-version tests.

- [ ] **Step 7: Commit Task 2**

```powershell
git add -- refresh.go runtime_wiring_test.go refresh_test.go
git diff --cached --check
git commit -m "fix: publish authoritative roster admission"
```

---

### Task 3: Verify, build, install, and perform browser acceptance

**Files:**
- Verify: all Go packages and refactor gates
- Build: `dist/codex-quota-scheduler.dll`
- Runtime: the configured CPA plugin installation and authenticated Management page

**Interfaces:**
- Consumes: completed Task 1 and Task 2 commits
- Produces: verified DLL and browser evidence for the approved behavior

- [ ] **Step 1: Run the full repository gate**

```powershell
./scripts/check_refactor_gates.ps1 -Stage S7
go test ./... -count=1 -timeout 180s
go test -race ./... -count=1 -timeout 300s
go vet ./...
git diff --check
```

Expected: every command exits `0`; S7 reports all exact Mock A-E owners passing.

- [ ] **Step 2: Build and hash the DLL**

```powershell
./build.ps1
Get-Item ./dist/codex-quota-scheduler.dll
Get-FileHash -Algorithm SHA256 ./dist/codex-quota-scheduler.dll
```

Expected: build exits `0`; DLL and generated header exist; record the SHA-256 hash.

- [ ] **Step 3: Install only into the already authorized local CPA instance**

Locate the currently installed scheduler DLL, compare hashes, replace it with the newly built artifact, and restart/reload the local CPA service using the same previously authorized local workflow. Do not modify unrelated plugins or external systems.

- [ ] **Step 4: Perform authenticated browser acceptance**

Using the existing in-app browser session at `http://localhost:8317/management.html#/plugin-pages/codex-quota-scheduler/0`:

1. Confirm roster health is Healthy or allowed Degraded and `background_allowed=true`.
2. Trigger global manual quota refresh.
3. Confirm `last_auth_scan` becomes non-empty and `codex_auth_count` equals the active highest-tier Codex roster count.
4. Confirm each admitted account makes a real refresh and displays populated LongWindow quota.
5. Confirm accounts with absent FiveHour but valid LongWindow are available and show no FiveHour row.
6. Confirm any account lacking LongWindow remains unavailable/Unknown so CPA fallback applies.
7. Exercise per-account refresh, settings GET/save confirmation, logs refresh/export, and configuration export.
8. Inspect logs for unexpected refresh/probe/admission errors and verify lower tiers/other providers never appear.

- [ ] **Step 5: Re-run a focused smoke gate after runtime testing**

```powershell
go test . -run 'TestParseCodexUsagePayloadAllowsSecondaryOnlyLongWindow|TestPickAllows(Weekly|Monthly)WithoutFiveHour|TestProductionRosterPublicationEnablesManualRefresh' -count=1
git status --short --branch
```

Expected: tests pass; tracked worktree is clean and only the preserved untracked workflow artifacts remain.
