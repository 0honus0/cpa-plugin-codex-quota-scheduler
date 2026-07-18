# First-Observation Lazy Window Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Immediately precheck and activate a newly observed unused quota window whose reset timestamp is anchored to the observation time plus one full window duration.

**Architecture:** Persist an explicit `SuspectedLazy` marker in `ProbeBaseline`, set it only during first-observation bootstrap under a strict zero-usage/duration/reset-anchor predicate, and consume it before the ordinary future-reset classification. Preserve the existing single-lease Probe sequence, WAL, send fence, propagation wait, retry suppression, roster authority, and credential validation.

**Tech Stack:** Go 1.26, Go tests, PowerShell S7 gate, CGO `c-shared` Windows build.

## Global Constraints

- Work only on branch `codex/fix-first-observation-probe` in the existing linked worktree.
- Do not modify the user's main checkout or the pre-existing untracked `docs/superpowers/plans/2026-07-13-s7-final-review-fixes.md`.
- Automatic activation must remain gated by `enable_reset_probe`.
- A suspected first-observation lazy window requires explicit `used_percent == 0`, a known duration, and `abs(reset_at - (now + duration)) <= 3m`.
- Five-hour duration falls back to 5h; weekly duration falls back to 7d; monthly requires positive `limit_window_seconds`.
- Missing usage, non-zero usage, unknown duration, or reset outside tolerance keeps `WaitingReset` behavior.
- Never bypass roster authority, credential validation, single-flight, WAL recovery, verify-first recovery, or resend suppression.
- Update `docs/fable-spec/refactor-decision-spec.md` so the implementation remains spec-compliant.

---

### Task 1: Mark suspected lazy windows during bootstrap

**Files:**
- Modify: `probe_classify.go`
- Modify: `probe.go`
- Modify: `probe_runtime.go`
- Test: `probe_runtime_test.go`

**Interfaces:**
- Consumes: `QuotaWindow`, `probeWindowDuration`, `resetProbeCloseThreshold`, `ProbeBaseline`, and `ProbeWindow`.
- Produces: `ProbeBaseline.SuspectedLazy bool` and `firstObservationLazyWindow(now time.Time, window QuotaWindow) (time.Duration, bool)`.

- [ ] **Step 1: Write failing bootstrap classification tests**

Add a table-driven test to `probe_runtime_test.go`:

```go
func TestProbeBootstrapSchedulesFirstObservationLazyWindowsImmediately(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	fiveSeconds := int64(5 * time.Hour / time.Second)
	weekSeconds := int64(7 * 24 * time.Hour / time.Second)
	monthSeconds := int64(30 * 24 * time.Hour / time.Second)
	zero := 0.0

	tests := []struct {
		name   string
		kind   ProbeWindowKind
		window QuotaWindow
	}{
		{"five-hour", ProbeWindowFiveHour, QuotaWindow{Kind: WindowFiveHour, UsedPercent: &zero, LimitWindowSeconds: &fiveSeconds, ResetAt: now.Add(5 * time.Hour)}},
		{"weekly", ProbeWindowLong, QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7 * 24 * time.Hour)}},
		{"monthly", ProbeWindowLong, QuotaWindow{Kind: WindowMonthly, UsedPercent: &zero, LimitWindowSeconds: &monthSeconds, ResetAt: now.Add(30 * 24 * time.Hour)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newDueProbeRuntime(t, now, newProbeFixtureHost())
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			r.probeController.RemoveWindow(binding.Instance, tt.kind)
			quota := ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &tt.window}
			if tt.kind == ProbeWindowFiveHour {
				quota.FiveHour = &tt.window
				quota.LongWindow = &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(24 * time.Hour)}
			}
			r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: quota.Family, LastSuccessAt: now, Quota: quota})

			if err := r.bootstrapProbeWindows(); err != nil {
				t.Fatal(err)
			}
			window, ok := r.probeController.Window(binding.Instance, tt.kind)
			if !ok || window.State != ProbePendingCheck || !window.Baseline.SuspectedLazy || window.Baseline.WindowLength <= 0 {
				t.Fatalf("window = %#v, ok=%v", window, ok)
			}
		})
	}
}
```

Add this negative table test:

```go
func TestProbeBootstrapRejectsFirstObservationLazyFalsePositives(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	zero, used := 0.0, 1.0
	weekSeconds := int64(7 * 24 * time.Hour / time.Second)

	tests := []struct {
		name   string
		window QuotaWindow
	}{
		{"non-zero-usage", QuotaWindow{Kind: WindowWeekly, UsedPercent: &used, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7 * 24 * time.Hour)}},
		{"missing-usage", QuotaWindow{Kind: WindowWeekly, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7 * 24 * time.Hour)}},
		{"monthly-duration-unknown", QuotaWindow{Kind: WindowMonthly, UsedPercent: &zero, ResetAt: now.Add(30 * 24 * time.Hour)}},
		{"anchor-outside-tolerance", QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7*24*time.Hour + 10*time.Minute)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newDueProbeRuntime(t, now, newProbeFixtureHost())
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
			r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: now, Quota: ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &tt.window}})
			if err := r.bootstrapProbeWindows(); err != nil {
				t.Fatal(err)
			}
			window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
			if !ok || window.State != ProbeWaitingReset || window.Baseline.SuspectedLazy {
				t.Fatalf("window = %#v, ok=%v", window, ok)
			}
		})
	}
}
```

- [ ] **Step 2: Run the bootstrap tests to verify RED**

Run:

```powershell
go test . -run '^TestProbeBootstrap(SchedulesFirstObservationLazyWindowsImmediately|RejectsFirstObservationLazyFalsePositives)$' -count=1 -v
```

Expected: compilation fails because `ProbeBaseline.SuspectedLazy` does not exist, or assertions fail because bootstrap returns `WaitingReset`.

- [ ] **Step 3: Add the persisted marker and strict predicate**

In `probe_classify.go`, extend `ProbeBaseline`:

```go
type ProbeBaseline struct {
	Kind            ProbeBaselineKind `json:"kind"`
	ResetAt         time.Time         `json:"reset_at,omitempty"`
	Usage           float64           `json:"usage"`
	NextRecheckAt   time.Time         `json:"next_recheck_at,omitempty"`
	WindowLength    time.Duration     `json:"window_length,omitempty"`
	CandidateLength time.Duration     `json:"candidate_length,omitempty"`
	StableIntervals int               `json:"stable_intervals,omitempty"`
	SuspectedLazy   bool              `json:"suspected_lazy,omitempty"`
}
```

In `probe.go`, add:

```go
func firstObservationLazyWindow(now time.Time, window QuotaWindow) (time.Duration, bool) {
	if window.UsedPercent == nil || *window.UsedPercent != 0 || window.ResetAt.IsZero() {
		return 0, false
	}
	duration, ok := probeWindowDuration(window)
	if !ok || duration <= 0 {
		return 0, false
	}
	return duration, absDuration(window.ResetAt.Sub(now.Add(duration))) <= resetProbeCloseThreshold
}
```

- [ ] **Step 4: Wire the predicate into bootstrap**

In `bootstrapProbeWindows`, derive duration with `probeWindowDuration`, store it in the reset baseline, and replace the initial state selection with:

```go
	duration, durationKnown := probeWindowDuration(*pair.w)
	base := ResetProbeBaseline(pair.w.ResetAt, usage, 0)
	if durationKnown {
		base.WindowLength = duration
	}
	state := ProbeWaitingReset
	deadline := pair.w.ResetAt.Add(probeRefreshAfterResetDelay)
	if _, lazy := firstObservationLazyWindow(now, *pair.w); lazy {
		base.SuspectedLazy = true
		state = ProbePendingCheck
		deadline = time.Time{}
	} else if !deadline.After(now) {
		state = ProbePendingCheck
		deadline = time.Time{}
	}
```

Keep the existing roster fallback that changes the state to `ProbeWaitingRoster` and clears the deadline.

- [ ] **Step 5: Run bootstrap tests to verify GREEN**

Run:

```powershell
go test . -run '^TestProbeBootstrap(SchedulesFirstObservationLazyWindowsImmediately|RejectsFirstObservationLazyFalsePositives|RecreatesReappearedFiveHour)$' -count=1 -v
```

Expected: PASS.

- [ ] **Step 6: Commit bootstrap detection**

```powershell
git add probe_classify.go probe.go probe_runtime.go probe_runtime_test.go
git diff --cached --check
git commit -m "fix(probe): detect fresh lazy windows"
```

### Task 2: Consume suspected-lazy evidence during precheck

**Files:**
- Modify: `probe_classify.go`
- Test: `probe_classify_test.go`
- Test: `testdata/probe_classify_golden.json`

**Interfaces:**
- Consumes: `ProbeBaseline.SuspectedLazy`, `ProbeClassificationKind`, `probeSkewTolerance`, and `QuotaSnapshot`.
- Produces: suspected-lazy classification before the normal future-reset not-due rule.

- [ ] **Step 1: Write failing focused classifier tests**

Add to `probe_classify_test.go`:

```go
func TestClassifySuspectedFirstObservationLazyWindow(t *testing.T) {
	now := time.Date(2026, 7, 18, 23, 0, 0, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	zero := 0.0
	used := 1.0
	base := ResetProbeBaseline(reset, 0, 7*24*time.Hour)
	base.SuspectedLazy = true

	tests := []struct {
		name  string
		snap  QuotaSnapshot
		want  ProbeClassificationKind
	}{
		{"unchanged-zero-is-lazy", QuotaSnapshot{Valid: true, ResetAt: &reset, Usage: &zero}, ProbeStillLazy},
		{"usage-proves-active", QuotaSnapshot{Valid: true, ResetAt: &reset, Usage: &used}, ProbeActivatedInferred},
		{"moved-reset-proves-active", QuotaSnapshot{Valid: true, ResetAt: ptrTime(reset.Add(time.Hour)), Usage: &zero}, ProbeActivatedNew},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyProbeWindow(base, tt.snap, now)
			if got.Kind != tt.want {
				t.Fatalf("kind = %s, want %s", got.Kind, tt.want)
			}
			if got.Kind != ProbeStillLazy && got.Baseline.SuspectedLazy {
				t.Fatal("confirmed baseline retained suspected-lazy marker")
			}
		})
	}
}
```

- [ ] **Step 2: Run focused classifier test to verify RED**

Run:

```powershell
go test . -run '^TestClassifySuspectedFirstObservationLazyWindow$' -count=1 -v
```

Expected: unchanged future reset is classified `not_due_yet`, and non-zero usage is not confirmed.

- [ ] **Step 3: Implement the suspected-lazy branch**

In `ClassifyProbeWindow`, after validating the snapshot but before the ordinary baseline tables, add a dedicated branch for `base.SuspectedLazy`:

```go
	if base.SuspectedLazy {
		if snap.ResetAt != nil && snap.ResetAt.After(base.ResetAt.Add(probeSkewTolerance)) {
			base.ResetAt = *snap.ResetAt
			base.Usage = *snap.Usage
			base.SuspectedLazy = false
			return ProbeClassification{Kind: ProbeActivatedNew, Baseline: base}
		}
		if *snap.Usage > 0 {
			base.Usage = *snap.Usage
			base.SuspectedLazy = false
			return ProbeClassification{Kind: ProbeActivatedInferred, Baseline: base}
		}
		if snap.ResetAt != nil && absDuration(snap.ResetAt.Sub(base.ResetAt)) <= probeSkewTolerance {
			return ProbeClassification{Kind: ProbeStillLazy, Baseline: base}
		}
	}
```

Let contradictory/missing reset evidence fall through to the existing anomaly/ambiguous logic. Clear `SuspectedLazy` in every `ActivatedNew` or `ActivatedInferred` path before returning.

- [ ] **Step 4: Regenerate and inspect the classifier golden file**

Run the repository's existing golden regeneration mechanism used by `probe_classify_test.go`, then inspect the diff:

```powershell
$env:UPDATE_PROBE_GOLDEN='1'
go test . -run '^TestProbeClassifyGolden$' -count=1
Remove-Item Env:UPDATE_PROBE_GOLDEN
git diff -- testdata/probe_classify_golden.json
```

Expected: the existing 864-row table remains stable because it does not set the
new explicit marker. The new focused test owns suspected-lazy rows.

- [ ] **Step 5: Run classifier tests to verify GREEN**

```powershell
go test . -run 'Test(ClassifySuspectedFirstObservationLazyWindow|ProbeClassification)' -count=1 -v
```

Expected: PASS.

- [ ] **Step 6: Commit classifier behavior**

```powershell
git add probe_classify.go probe_classify_test.go testdata/probe_classify_golden.json
git diff --cached --check
git commit -m "fix(probe): precheck fresh lazy windows"
```

### Task 3: Prove the complete activation sequence remains single-flight

**Files:**
- Modify: `probe_runtime_test.go`
- Verify: `probe_runtime.go`
- Verify: `coordinator.go`
- Verify: `probe_wal.go`

**Interfaces:**
- Consumes: bootstrap marker, classifier behavior, `RunProbeRecoveryOnce`, and the existing `sequenceProbeHost` request recorder.
- Produces: end-to-end regression evidence for one precheck, one activation POST, and one verify read.

- [ ] **Step 1: Add an end-to-end weekly incident test**

Create a test using the existing production Probe fixture and `sequenceProbeHost`:

```go
func TestFirstObservedWeeklyLazyWindowSendsOneActivationAndVerifies(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{lazy, active}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowFiveHour)
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
	zero := 0.0
	seconds := int64(604800)
	r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: now, Quota: ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: reset}}})
	if err := r.bootstrapProbeWindows(); err != nil {
		t.Fatal(err)
	}
	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	posts := 0
	for _, url := range host.urls {
		if url == codexResetProbeEndpoint {
			posts++
		}
	}
	host.mu.Unlock()
	if posts != 1 {
		t.Fatalf("probe POST count = %d, want 1; urls=%v", posts, host.urls)
	}
	window, _ := r.probeController.Window(binding.Instance, ProbeWindowLong)
	if window.State != ProbeConfirmed || window.Baseline.SuspectedLazy {
		t.Fatalf("window = %#v", window)
	}
}
```

The existing `sequenceProbeHost` and `newDueProbeRuntime` are sufficient; do not
add a second fake-host implementation.

- [ ] **Step 2: Add concurrent-trigger coverage**

Create the same weekly suspected-lazy setup, then replace the propagation wait:

```go
	entered := make(chan struct{})
	release := make(chan struct{})
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-entered
	for i := 0; i < 4; i++ {
		if err := r.RunProbeDueOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	host.mu.Lock()
	posts := 0
	for _, url := range host.urls {
		if url == codexResetProbeEndpoint {
			posts++
		}
	}
	host.mu.Unlock()
	if posts != 1 {
		t.Fatalf("probe POST count during propagation = %d, want 1", posts)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if attempt, ok := persisted.ProbeAttempts[binding.Instance]; ok && nonterminalProbeAttempt(attempt) {
		t.Fatalf("nonterminal attempt survived: %#v", attempt)
	}
```

- [ ] **Step 3: Run end-to-end tests**

```powershell
go test . -run 'TestFirstObservedWeeklyLazyWindow(SendsOneActivationAndVerifies|ConcurrentTriggersSingleFlight)$' -count=1 -v -timeout 120s
```

Expected: PASS with exactly one activation POST per account instance.

- [ ] **Step 4: Run existing recovery and coordinator regressions**

```powershell
go test . -run 'Test(ProbeSequence|ProbeRecovery|TypedCoordinator|FirstObservedWeeklyLazyWindow)' -count=1 -timeout 180s
```

Expected: PASS.

- [ ] **Step 5: Commit sequence regressions**

```powershell
git add probe_runtime_test.go
git diff --cached --check
git commit -m "test(probe): cover fresh lazy activation"
```

### Task 4: Amend the decision spec and run release-grade verification

**Files:**
- Modify: `docs/fable-spec/refactor-decision-spec.md`
- Verify: all changed Go and test files.

**Interfaces:**
- Consumes: the approved 2026-07-18 design and implemented behavior.
- Produces: spec-compliant documentation, full verification evidence, and a Windows DLL for local testing.

- [ ] **Step 1: Amend the first-observation classification table**

Before the current rule “no baseline, future reset -> WaitingReset”, add a rule stating:

```text
No baseline; explicit usage is 0; window length is known; and
abs(reset_at - (observation_time + window_length)) <= 3m:
persist SuspectedLazy and enter PendingCheck immediately.
```

Add a paragraph stating that suspected-lazy precheck classification runs before the ordinary future-reset not-due rule, and that moved reset or non-zero usage confirms activation without a send.

- [ ] **Step 2: Run focused Probe verification**

```powershell
go test . -run 'Test(ProbeBootstrap|ClassifySuspected|FirstObservedWeeklyLazyWindow|ProbeSequence|ProbeRecovery)' -count=1 -timeout 180s
```

Expected: PASS.

- [ ] **Step 3: Run full verification gates**

```powershell
git diff --check
go test ./... -count=1 -timeout 300s
go test -race ./... -count=1 -timeout 300s
go vet ./...
.\scripts\check_refactor_gates.ps1 -Stage S7
```

Expected: all commands exit 0; S7 reports all exact Mock row owners passed.

- [ ] **Step 4: Build the Windows amd64 DLL**

```powershell
.\build.ps1
Get-FileHash -Algorithm SHA256 .\dist\codex-quota-scheduler.dll
```

Expected: `dist/codex-quota-scheduler.dll` exists and a SHA-256 hash is printed.

- [ ] **Step 5: Commit the spec amendment**

```powershell
git add docs/fable-spec/refactor-decision-spec.md
git diff --cached --check
git commit -m "docs: specify fresh lazy probe handling"
```

- [ ] **Step 6: Inspect final branch scope**

```powershell
git status --short
git log --oneline --decorate -10
git diff origin/main...HEAD --stat
```

Expected: only the pre-existing untracked S7 plan remains; all fix files are committed on `codex/fix-first-observation-probe`.
