# Probe Production Wake And Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run a first-observation lazy-window Probe immediately after production quota refresh and expose its lifecycle in the Management scheduling log.

**Architecture:** Keep `PendingCheck` deadlines cleared and hand newly runnable work directly from quota-refresh writeback to the existing asynchronous Probe runner. Preserve the S6 coordinator, WAL, send fence, and `probeRunMu` as duplicate-send authorities. Record lifecycle logs from the single-lease Probe sequence without making logs part of state recovery.

**Tech Stack:** Go 1.26, Go `testing`, CPA plugin ABI v7, PowerShell S7 refactor gate, CGO `c-shared` Windows build.

## Global Constraints

- Work only in `.worktrees/superpowers-refactor-spec-plan` on `codex/fix-first-observation-probe`.
- Do not change the main checkout or delete `docs/superpowers/plans/2026-07-13-s7-final-review-fixes.md`.
- Keep `PendingCheck.Deadline` zero.
- Keep `enable_reset_probe`, authoritative-roster, credential, WAL, fence, single-flight, and verify-first gates intact.
- Never log credentials, request bodies, quota response bodies, or unredacted upstream errors.
- A production refresh must launch Probe work without tests directly calling `RunProbeDueOnce`.

---

### Task 1: Wake Probe From Production Refresh

**Files:**
- Modify: `refresh.go:809-838`
- Test: `probe_runtime_test.go`

**Interfaces:**
- Consumes: `(*QuotaRefresher).bootstrapProbeWindows() error`, `(*QuotaRefresher).launchProbe(bool)`, `(*ProbeController).Snapshot()`.
- Produces: `hasPendingProbeWindows(map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow) bool`.

- [ ] **Step 1: Write the failing production-chain test**

Add a test that starts a production refresher, performs a real manual refresh, and waits for the Probe POST without directly invoking `RunProbeDueOnce`:

```go
func TestProductionRefreshLaunchesFirstObservedLazyProbe(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 18, 49, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{lazy, lazy, active}
	host.doStarted = make(chan struct{})
	r := newProductionLazyRefreshRuntime(t, now, host)
	r.Start()
	t.Cleanup(r.Stop)

	if err := r.RefreshOneAuthID("a"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.doStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("production refresh did not launch Probe")
	}
	eventually(t, 5*time.Second, func() bool {
		posts, _ := probePOSTCount(host)
		return posts == 1
	})
}
```

`newProductionLazyRefreshRuntime` must create the same authoritative roster and binding as `newDueProbeRuntime`, but it must not seed any Probe window. Its fake host queue represents refresh GET, precheck GET, and verify GET.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```powershell
go test ./... -run '^TestProductionRefreshLaunchesFirstObservedLazyProbe$' -count=1
```

Expected: FAIL with `production refresh did not launch Probe`; the refresh bootstrap creates `PendingCheck`, but the ordinary timer never launches it.

- [ ] **Step 3: Add the minimal pending-work predicate and production handoff**

Add beside the Probe refresh helpers:

```go
func hasPendingProbeWindows(windows map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow) bool {
	for _, instanceWindows := range windows {
		for _, window := range instanceWindows {
			if window.State == ProbePendingCheck {
				return true
			}
		}
	}
	return false
}
```

After successful `bootstrapProbeWindows()` in the coordinator apply callback:

```go
if err := r.bootstrapProbeWindows(); err != nil {
	r.state.RecordLog("warn", "probe.bootstrap_failed", "Probe 状态初始化失败，将在下次刷新重试", map[string]any{"auth_id": intent.AuthID, "error": redactSecrets(err.Error())}, r.now())
} else if hasPendingProbeWindows(r.probeController.Snapshot()) {
	r.launchProbe(false)
}
```

Do not assign a deadline to `PendingCheck` and do not call the Probe sequence synchronously from the coordinator apply callback.

- [ ] **Step 4: Run focused wake and concurrency tests and verify GREEN**

Run:

```powershell
go test ./... -run 'TestProductionRefreshLaunchesFirstObservedLazyProbe|TestFirstObservedWeeklyLazyWindowConcurrentTriggersSingleFlight' -count=1
```

Expected: PASS; production refresh sends one POST and repeated triggers remain single-flight.

- [ ] **Step 5: Commit Task 1**

```powershell
git add refresh.go probe_runtime_test.go
git commit -m "fix(probe): wake lazy checks after refresh"
```

---

### Task 2: Record Probe Lifecycle Logs

**Files:**
- Modify: `probe_runtime.go:514-710`
- Test: `probe_runtime_test.go`
- Test: `management_test.go`

**Interfaces:**
- Consumes: `(*PluginState).RecordLog(level, event, message string, fields map[string]any, now time.Time)` and `redactSecrets(string) string`.
- Produces: events `probe.precheck_started`, `probe.activation_sent`, `probe.verified`, and `probe.failed`.

- [ ] **Step 1: Write failing lifecycle-log tests**

Extend the successful first-observation test to assert exact event order:

```go
logs := r.state.Snapshot(now).Logs
events := make([]string, 0, len(logs))
for _, entry := range logs {
	if strings.HasPrefix(entry.Event, "probe.") {
		events = append(events, entry.Event)
	}
}
want := []string{"probe.precheck_started", "probe.activation_sent", "probe.verified"}
if !reflect.DeepEqual(events, want) {
	t.Fatalf("Probe events = %v, want %v", events, want)
}
```

Add a failure test with a precheck quota error and assert one terminal `probe.failed` entry whose `error` field does not contain access token, refresh token, account ID, authorization header, request body, or upstream response body.

- [ ] **Step 2: Run focused log tests and verify RED**

Run:

```powershell
go test ./... -run 'TestFirstObservedWeeklyLazyWindowSendsOneActivationAndVerifies|TestProbeFailureLogRedactsSecrets' -count=1
```

Expected: FAIL because the S6 sequence currently records no lifecycle events.

- [ ] **Step 3: Record logs at the authoritative phase boundaries**

In `OperationProbeSequence`:

```go
fields := func(windows []ProbeWindowKind) map[string]any {
	return map[string]any{"auth_id": intent.AuthID, "attempt_id": attempt.AttemptID, "windows": append([]ProbeWindowKind(nil), windows...)}
}
```

Record `probe.precheck_started` immediately before the precheck fence/read. Record `probe.activation_sent` only after `PersistSent` succeeds. Record `probe.verified` only after verify classification, WAL completion, observed-window reconciliation, and Probe-window persistence succeed. In `fail`, record `probe.failed` with `error: redactSecrets(err.Error())`, `sent`, and the affected windows after retry/auth-blocked state persistence has been attempted.

Use these messages:

```go
"开始检查疑似未激活的额度周期"
"已发送极小请求以激活额度周期"
"额度周期激活验证完成"
"额度周期探测失败，已按安全策略处理"
```

Recovery verification may emit `probe.verified`, but must never emit a second `probe.activation_sent`.

- [ ] **Step 4: Verify Management serialization and secret redaction**

Add lifecycle events containing sentinel-safe fields to the existing Management status/log redaction test. Request authenticated status and assert the event names and messages remain visible while sentinel credentials and response body text remain absent.

Run:

```powershell
go test ./... -run 'TestFirstObservedWeeklyLazyWindowSendsOneActivationAndVerifies|TestProbeFailureLogRedactsSecrets|TestManagement.*Log' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit Task 2**

```powershell
git add probe_runtime.go probe_runtime_test.go management_test.go
git commit -m "feat(probe): expose activation lifecycle logs"
```

---

### Task 3: Verify Spec Gates And Build DLL

**Files:**
- Modify only if verification exposes a regression in Task 1 or Task 2 files.
- Build output: `dist/codex-quota-scheduler.dll`.

**Interfaces:**
- Consumes: completed production wake and lifecycle logging changes.
- Produces: a tested Windows AMD64 CPA plugin DLL and its SHA-256.

- [ ] **Step 1: Run focused Probe suite**

```powershell
go test ./... -run 'Probe|FirstObserved|ProductionRefreshLaunchesFirstObservedLazyProbe' -count=1 -timeout 300s
```

Expected: PASS.

- [ ] **Step 2: Run full test, race, vet, and S7 gates**

```powershell
go test ./... -count=1 -timeout 300s
go test -race ./... -count=1 -timeout 300s
go vet ./...
& .\scripts\check_refactor_gates.ps1 -Stage S7
```

Expected: every command exits `0`; S7 reports all exact Mock row owners passing.

- [ ] **Step 3: Build and hash the DLL**

```powershell
& .\build.ps1
Get-Item .\dist\codex-quota-scheduler.dll | Select-Object FullName,Length,LastWriteTime
Get-FileHash .\dist\codex-quota-scheduler.dll -Algorithm SHA256
```

Expected: build exits `0`, DLL exists, and SHA-256 is printed.

- [ ] **Step 4: Commit any verification-only corrections**

If verification required source corrections, stage only the affected Task 1/Task 2 files and commit:

```powershell
git commit -m "fix(probe): satisfy production activation gates"
```

If no files changed, do not create an empty commit.
