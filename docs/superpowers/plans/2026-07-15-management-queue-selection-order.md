# Management Queue Selection Order Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Management account queue mirror production availability classes, ignore plugin priority for hard-excluded accounts, sort excluded accounts by recovery time, and show authoritative LongWindow exhaustion ahead of historical temporary feedback.

**Architecture:** Extract one pure `AccountState -> AccountView` projection shared by production snapshot publication and Management ordering. Add internal selection class/view fields to `ScheduledAccount`; selectable classes reuse `accountViewLess`, while `Excluded` uses a recovery-time comparator. Keep durable temporary feedback unchanged, but move its display classification after mandatory LongWindow validation and exhaustion.

**Tech Stack:** Go 1.24, CLIProxyAPI v7 plugin ABI, standard-library `sort`/`time`, existing Go test suite, PowerShell build/gate scripts, Windows amd64 c-shared DLL.

## Global Constraints

- Only highest-CPA-priority Codex roster members enter the queue.
- Missing Codex priority continues to normalize to `0`; non-Codex providers remain ignored.
- Missing FiveHour is valid only when LongWindow is valid; never synthesize FiveHour data.
- Missing or invalid LongWindow remains unavailable and delegates to CPA fallback.
- Production scheduler pick remains snapshot-only and performs no I/O.
- No new persistent state, schema migration, host callback, or OpenAI request.
- Preserve every untracked `.superpowers/sdd/*` artifact and unrelated user file.
- Real deployment target is `C:\CPA\CLIProxyAPI\plugins\windows\amd64\codex-quota-scheduler.dll`.
- CPA must be fully restarted after DLL replacement; plugin disable/enable alone reuses the old in-memory module.

---

### Task 1: Share production availability classes with Management ordering

**Files:**
- Modify: `scheduler_snapshot.go`
- Modify: `scheduler.go`
- Modify: `scheduler_test.go`
- Modify: `integration_test.go`
- Modify: `management.go`
- Test: `scheduler_test.go`
- Test: `integration_test.go`

**Interfaces:**
- Consumes: `AccountState`, `Config`, `TrialRegistry`, `AccountView`, `ClassifyAccount(AccountView, time.Time) AvailabilityClass`, `accountViewLess(AccountView, AccountView, MonthlyMode) bool`.
- Produces: `accountViewFromState(AccountState, Config, time.Time, *TrialRegistry) AccountView`; `buildOrderedAccounts(pluginapi.SchedulerPickRequest, StateSnapshot, time.Time, *TrialRegistry) []ScheduledAccount`; internal `ScheduledAccount.selectionClass AvailabilityClass`; internal `ScheduledAccount.selectionView AccountView`.

- [ ] **Step 1: Write failing ordering regressions**

Add focused tests with real `AccountState` values:

```go
func TestManagementQueueMatchesProductionAvailabilityClasses(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	used := 100.0
	exhaustedHigh := weeklyAccount("exhausted-high", 0, now.Add(2*time.Hour), false)
	exhaustedHigh.Quota.LongWindow.UsedPercent = &used
	exhaustedHigh.Quota.LongWindow.Exhausted = true
	exhaustedHigh.Annotation.SchedulerPriority = 100
	availableLow := weeklyAccount("available-low", 0, now.Add(4*time.Hour), false)
	availableLow.Annotation.SchedulerPriority = 0
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{exhaustedHigh, availableLow}}

	ordered := BuildOrderedAccounts(requestWithCandidates("exhausted-high", "available-low"), snapshot, now)
	if len(ordered) != 2 || ordered[0].AuthID != "available-low" || ordered[1].AuthID != "exhausted-high" {
		t.Fatalf("ordered = %#v", ordered)
	}
}

func TestExcludedManagementQueueIgnoresPluginPriorityAndSortsByRecovery(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	used := 100.0
	laterHigh := weeklyAccount("later-high", 0, now.Add(8*time.Hour), false)
	laterHigh.Quota.LongWindow.UsedPercent = &used
	laterHigh.Quota.LongWindow.Exhausted = true
	laterHigh.Annotation.SchedulerPriority = 100
	earlierLow := weeklyAccount("earlier-low", 0, now.Add(2*time.Hour), false)
	earlierLow.Quota.LongWindow.UsedPercent = &used
	earlierLow.Quota.LongWindow.Exhausted = true
	earlierLow.Annotation.SchedulerPriority = 0
	unknown := weeklyAccount("unknown", 0, now.Add(24*time.Hour), false)
	unknown.Refresh.AuthFailure = true
	unknown.Annotation.SchedulerPriority = 1000
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{laterHigh, earlierLow, unknown}}

	ordered := BuildOrderedAccounts(requestWithCandidates("later-high", "earlier-low", "unknown"), snapshot, now)
	want := []string{"earlier-low", "later-high", "unknown"}
	for i := range want {
		if ordered[i].AuthID != want[i] {
			t.Fatalf("ordered = %#v, want %v", ordered, want)
		}
	}
}

func TestSelectableManagementQueueKeepsPluginPriorityWithinClass(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	preferredLow := weeklyAccount("preferred-low", 0, now.Add(2*time.Hour), false)
	preferredHigh := weeklyAccount("preferred-high", 0, now.Add(8*time.Hour), false)
	preferredLow.Annotation.SchedulerPriority = 0
	preferredHigh.Annotation.SchedulerPriority = 10

	opportunisticLow := weeklyAccount("opportunistic-low", 0, now.Add(3*time.Hour), false)
	opportunisticHigh := weeklyAccount("opportunistic-high", 0, now.Add(9*time.Hour), false)
	opportunisticLow.LastSuccessAt = time.Time{}
	opportunisticHigh.LastSuccessAt = time.Time{}
	opportunisticLow.Annotation.SchedulerPriority = 0
	opportunisticHigh.Annotation.SchedulerPriority = 10

	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		preferredLow, preferredHigh, opportunisticLow, opportunisticHigh,
	}}
	ordered := BuildOrderedAccounts(requestWithCandidates(
		"preferred-low", "preferred-high", "opportunistic-low", "opportunistic-high",
	), snapshot, now)
	want := []string{"preferred-high", "preferred-low", "opportunistic-high", "opportunistic-low"}
	for i := range want {
		if ordered[i].AuthID != want[i] {
			t.Fatalf("ordered = %#v, want %v", ordered, want)
		}
	}
}
```

Extend the existing six-account incident test so its expected order is:

```go
wantIDs := []string{"usable-a", "usable-b", "usable-c", "usable-d", "exhausted-a", "exhausted-b"}
```

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```powershell
go test . -run 'Test(ManagementQueueMatchesProductionAvailabilityClasses|ExcludedManagementQueueIgnoresPluginPriorityAndSortsByRecovery|SixAccountIncidentUsesPluginPriorityFallthroughWhenCPAPrioritiesMatch)' -count=1
```

Expected: FAIL because `BuildOrderedAccounts` places plugin priority before availability and recovery time.

- [ ] **Step 3: Extract the shared AccountView projection**

Move the existing projection logic from `schedulerSnapshotFromState` into:

```go
func accountViewFromState(a AccountState, cfg Config, now time.Time, trials *TrialRegistry) AccountView {
	cache := CacheFresh
	if a.LastSuccessAt.IsZero() {
		cache = CacheUnknown
	} else if a.Stale {
		cache = CacheStale
	} else if now.Sub(a.LastSuccessAt) > cfg.QuotaRefreshInterval {
		cache = CacheAging
	}
	exhausted, reset := accountExhaustion(a, now)
	trial := TrialNone
	if trials != nil {
		trial = trials.State(a.Instance, now)
	}
	circuit := effectiveCircuitState(a.Circuit, now).EffectiveState
	circuitClass := CircuitClosed
	if circuit == CircuitStateOpen {
		circuitClass = CircuitOpen
	} else if circuit == CircuitStateHalfOpen {
		circuitClass = CircuitHalfOpen
	}
	return AccountView{
		ID: a.AuthID, AuthIndex: a.AuthIndex, Instance: a.Instance,
		PluginPriority: a.Annotation.SchedulerPriority, Family: a.Family,
		Cache: cache, LastKnownAvailable: a.LastError == "", Exhausted: exhausted,
		ResetAt: reset, AuthBlocked: a.Refresh.AuthFailure, Circuit: circuitClass,
		TemporaryUnavailable: a.TemporaryExhausted && a.TemporaryResetAt.After(now),
		Trial: trial, Expiry: accountSortTime(a), RemainingQuota: remainingQuota(a),
	}
}
```

Replace the inline production projection with:

```go
accounts = append(accounts, accountViewFromState(a, state.Config, state.Now, trials))
```

- [ ] **Step 4: Implement class-first Management ordering**

Extend `ScheduledAccount` internally:

```go
selectionClass AvailabilityClass
selectionView  AccountView
```

Rename the current `BuildOrderedAccounts` implementation to
`buildOrderedAccounts`, add `trials *TrialRegistry` to that implementation's
parameters, and place this compatibility wrapper immediately above it:

```go
func BuildOrderedAccounts(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time) []ScheduledAccount {
	return buildOrderedAccounts(req, snapshot, now, nil)
}
```

For every known account, populate the shared selection values:

```go
view := accountViewFromState(account, snapshot.Config, now, trials)
scheduled.selectionView = view
scheduled.selectionClass = ClassifyAccount(view, now)
```

Unknown accounts receive `selectionClass: Excluded` and a selection view whose
ID is the candidate ID.

Replace the comparator with:

```go
sort.SliceStable(ordered, func(i, j int) bool {
	left, right := ordered[i], ordered[j]
	if left.selectionClass != right.selectionClass {
		return left.selectionClass < right.selectionClass
	}
	if left.selectionClass != Excluded {
		return accountViewLess(left.selectionView, right.selectionView, snapshot.Config.MonthlyMode)
	}
	if !left.SortTime.Equal(right.SortTime) {
		if left.SortTime.IsZero() {
			return false
		}
		if right.SortTime.IsZero() {
			return true
		}
		return left.SortTime.Before(right.SortTime)
	}
	return left.AuthID < right.AuthID
})
```

Use `buildOrderedAccounts(..., globalTrials)` in both authenticated Management
status builders. Keep `PickCodexAccount` and test helpers on the public wrapper
unless a test explicitly supplies a trial registry.

- [ ] **Step 5: Run ordering tests and verify GREEN**

Run:

```powershell
go test . -run 'Test(ManagementQueue|ExcludedManagementQueue|SixAccountIncident|Pick.*Priority|SelectAccount)' -count=1
```

Expected: PASS. Preferred accounts precede Opportunistic; Opportunistic precede Excluded; plugin priority orders only selectable classes; Excluded sorts by recovery with unknown recovery last.

- [ ] **Step 6: Commit Task 1**

```powershell
git add scheduler_snapshot.go scheduler.go scheduler_test.go integration_test.go management.go
git commit -m "fix: order management queue by availability"
```

---

### Task 2: Prefer authoritative LongWindow exhaustion over temporary feedback

**Files:**
- Modify: `scheduler.go`
- Modify: `scheduler_test.go`
- Modify: `management_test.go`
- Test: `scheduler_test.go`
- Test: `management_test.go`

**Interfaces:**
- Consumes: `accountQueueState(AccountState, time.Time) (QueueStatus, bool, string, time.Time)`, `windowExhausted(*QuotaWindow, time.Time) bool`.
- Produces: unchanged signature with mandatory LongWindow validation/exhaustion preceding active `TemporaryExhausted` feedback.

- [ ] **Step 1: Write failing status precedence tests**

```go
func TestLongWindowExhaustionPrecedesTemporaryFeedbackWithoutFiveHour(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	used := 100.0
	weekly := weeklyAccount("weekly", 0, now.Add(6*time.Hour), false)
	weekly.Quota.FiveHour = nil
	weekly.Quota.LongWindow.UsedPercent = &used
	weekly.Quota.LongWindow.Exhausted = true
	weekly.TemporaryExhausted = true
	weekly.TemporaryResetAt = now.Add(time.Hour)

	status, available, reason, resetAt := accountQueueState(weekly, now)
	if status != QueueStatusLongWindowExhausted || available || reason != "weekly_exhausted" || !resetAt.Equal(weekly.Quota.LongWindow.ResetAt) {
		t.Fatalf("state = %q/%v/%q/%v", status, available, reason, resetAt)
	}
}

func TestMonthlyLongWindowExhaustionPrecedesTemporaryFeedback(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	used := 100.0
	monthly := monthlyAccount("monthly", 0, now.Add(6*time.Hour))
	monthly.Quota.LongWindow.UsedPercent = &used
	monthly.Quota.LongWindow.Exhausted = true
	monthly.TemporaryExhausted = true
	monthly.TemporaryResetAt = now.Add(time.Hour)

	status, available, reason, resetAt := accountQueueState(monthly, now)
	if status != QueueStatusLongWindowExhausted || available || reason != "monthly_exhausted" || !resetAt.Equal(monthly.Quota.LongWindow.ResetAt) {
		t.Fatalf("state = %q/%v/%q/%v", status, available, reason, resetAt)
	}
}

func TestTemporaryFeedbackStillAppliesWhenLongWindowIsAvailable(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	weekly := weeklyAccount("weekly", 0, now.Add(6*time.Hour), false)
	weekly.Quota.FiveHour = nil
	weekly.TemporaryExhausted = true
	weekly.TemporaryResetAt = now.Add(time.Hour)

	status, available, reason, resetAt := accountQueueState(weekly, now)
	if status != QueueStatusFiveHourExhausted || available || reason != "temporary_exhausted" || !resetAt.Equal(weekly.TemporaryResetAt) {
		t.Fatalf("state = %q/%v/%q/%v", status, available, reason, resetAt)
	}
}

func TestManagementReportsLongWindowExhaustionAheadOfTemporaryFeedback(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	used := 100.0
	account := weeklyAccount("weekly", 0, now.Add(6*time.Hour), false)
	account.Quota.FiveHour = nil
	account.Quota.LongWindow.UsedPercent = &used
	account.Quota.LongWindow.Exhausted = true
	account.TemporaryExhausted = true
	account.TemporaryResetAt = now.Add(time.Hour)
	snapshot := StateSnapshot{Config: DefaultConfig(), Accounts: []AccountState{account}, Now: now}

	payload := BuildStatusPayload(snapshot, BuildOrderedAccounts(requestWithCandidates("weekly"), snapshot, now))
	if len(payload.Accounts) != 1 || payload.Accounts[0].UnavailableReason != "weekly_exhausted" {
		t.Fatalf("accounts = %#v", payload.Accounts)
	}
}
```

- [ ] **Step 2: Run precedence tests and verify RED**

Run:

```powershell
go test . -run 'Test(LongWindowExhaustionPrecedesTemporaryFeedbackWithoutFiveHour|TemporaryFeedbackStillAppliesWhenLongWindowIsAvailable|Management.*LongWindow.*Temporary)' -count=1
```

Expected: the exhausted-LongWindow tests FAIL with `temporary_exhausted`; the available-LongWindow characterization remains PASS.

- [ ] **Step 3: Move temporary feedback behind LongWindow validation**

Remove the top-level temporary block from `accountQueueState`. Inside both
Weekly and Monthly branches, keep the required LongWindow checks first, then
insert the unchanged durable temporary check immediately after LongWindow
exhaustion:

```go
if windowExhausted(account.Quota.LongWindow, now) {
	return QueueStatusLongWindowExhausted, false, "weekly_exhausted", account.Quota.LongWindow.ResetAt
}
if account.TemporaryExhausted && (account.TemporaryResetAt.IsZero() || account.TemporaryResetAt.After(now)) {
	return QueueStatusFiveHourExhausted, false, "temporary_exhausted", account.TemporaryResetAt
}
```

Use `monthly_exhausted` in the Monthly LongWindow branch. Do not mutate or clear
`TemporaryExhausted`, `TemporaryResetAt`, or its stored reason.

- [ ] **Step 4: Run precedence and optional-FiveHour tests and verify GREEN**

Run:

```powershell
go test . -run 'Test(LongWindowExhaustion|TemporaryFeedback|PickAllowsWeeklyWithoutFiveHour|PickRejectsAccountWithoutLongWindow|Management.*FiveHour)' -count=1
```

Expected: PASS. Weekly/Monthly long exhaustion is authoritative; temporary feedback still blocks an otherwise valid account; optional FiveHour behavior is unchanged.

- [ ] **Step 5: Commit Task 2**

```powershell
git add scheduler.go scheduler_test.go management_test.go
git commit -m "fix: prefer authoritative quota exhaustion"
```

---

### Task 3: Review, verify, build, deploy, and accept against real CPA

**Files:**
- Verify: all tracked Go and documentation files
- Build: `dist/codex-quota-scheduler.dll`
- Deploy: `C:\CPA\CLIProxyAPI\plugins\windows\amd64\codex-quota-scheduler.dll`

**Interfaces:**
- Consumes: committed Task 1 and Task 2 behavior, `scripts/check_refactor_gates.ps1`, `build.ps1`, authenticated localhost Management routes.
- Produces: independently reviewed commits, verified Windows DLL, matching installed hash, restarted CPA process, and browser/API evidence of the requested queue order.

- [ ] **Step 1: Request independent code review**

Provide the reviewer the DEV-012 spec, Task 1/2 commit range, and these required checks:

```text
- Preferred -> Opportunistic -> Excluded matches production classification.
- Plugin priority is ignored only for Excluded.
- Excluded recovery sorting puts unknown recovery last.
- LongWindow exhaustion precedes temporary feedback without clearing feedback state.
- CPA admission, optional FiveHour, snapshot-only pick, and Management security boundaries remain unchanged.
```

Fix every Critical or Important finding through a new RED -> GREEN cycle and request re-review.

- [ ] **Step 2: Run the full verification gate**

```powershell
.\scripts\check_refactor_gates.ps1 -Stage S7
go test ./... -count=1 -timeout 180s
go test -race ./... -count=1 -timeout 300s
go vet ./...
git diff --check
.\build.ps1
Get-FileHash .\dist\codex-quota-scheduler.dll -Algorithm SHA256
```

Expected: all commands exit `0`; S7 reports all exact Mock owners passed; the DLL hash is non-empty.

- [ ] **Step 3: Deploy the verified DLL**

Disable the plugin through the authenticated local Management API, preserve the
current installed DLL as a uniquely named backup if that hash is not already
backed up, copy the verified DLL to the real plugin directory, and confirm the
installed hash equals the build hash.

Restart the LocalSystem `CliProxyAPI` NSSM service. If the current shell lacks
service-control permission, request the existing narrow UAC action that runs:

```powershell
Restart-Service -Name CliProxyAPI -Force
```

Wait until port `8317` is listening under a new `cli-proxy-api.exe` process.

- [ ] **Step 4: Verify API ordering after a real refresh**

Use the authenticated localhost status and refresh routes. Confirm:

```text
- roster admission remains observed and admits all six priority-0 Codex accounts;
- manual refresh returns 202 and codex_auth_count returns to 6;
- all selectable accounts precede all exhausted accounts;
- selectable accounts still use plugin priority and existing tie-breakers;
- excluded accounts ignore plugin priority and increase by known recovery time;
- the exhausted missing-FiveHour account reports weekly_exhausted;
- missing LongWindow remains unavailable in automated coverage.
```

- [ ] **Step 5: Verify the visible Management page**

Reload the existing in-app browser tab, load authenticated data, and confirm the
card order visually. The current incident should place cards 1, 2, 5, and 6
ahead of cards 3 and 4; cards 3 and 4 should then sort by their Weekly reset
times. Confirm no missing FiveHour row is rendered.

- [ ] **Step 6: Final handoff**

Report the final commit IDs, DLL path/hash, full verification evidence, reviewer
verdict, live queue order, status reasons, and the requirement to restart CPA
after future DLL replacements. Preserve all unrelated untracked artifacts.
