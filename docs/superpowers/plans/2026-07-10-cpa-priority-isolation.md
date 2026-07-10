# CPA Priority Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Release v0.1.6 with a highest-CPA-priority admission boundary, an independent plugin-owned priority, internal tier fallback, and protection against repeated refreshes from an already-consumed reset timestamp.

**Architecture:** Each Codex scheduler request replaces an in-memory admission set with only the maximum CPA priority tier. Every refresh, state, management, and scheduling path uses that set. Admitted accounts are ordered by a persisted annotation field named `scheduler_priority`; unavailable high plugin tiers fall through to lower plugin tiers before fallback. Reset-trigger refreshes become one-shot by comparing the trigger timestamp with the last successful refresh.

**Tech Stack:** Go 1.26, CLIProxyAPI v7.2.42 plugin SDK, Go `testing`, embedded HTML/JavaScript management UI, GitHub CLI, GitHub Actions.

## Global Constraints

- Do not modify CPA source code.
- Create one upstream CPA issue and one linked remote plugin issue.
- Load only accounts in the maximum CPA priority tier.
- Recommend identical CPA priorities for all managed accounts, preferably `0`.
- Plugin priority defaults to `0`, larger integers are preferred, and it never reads from or writes to CPA priority.
- Preserve the completed local v0.1.5 commits and tag.
- Target release version is exactly v0.1.6.
- Write and observe a failing test before each production change.
- Do not recreate either deleted experimental worktree or branch.

---

### Task 1: Create the linked remote issues

**Files:** None.

**Interfaces:**
- Consumes: GitHub repositories `router-for-me/CLIProxyAPI` and `JefferyZhang2019/cpa-plugin-codex-quota-scheduler`.
- Produces: an upstream CPA issue URL and a linked plugin issue URL.

- [ ] **Step 1: Check for an existing upstream duplicate**

Run:

```powershell
gh issue list -R router-for-me/CLIProxyAPI --state all --limit 100 --search 'scheduler priority fallback exhausted tier'
```

Expected: no exact issue for built-in fallback stopping at an exhausted highest auth-priority tier. If an exact duplicate exists, use its URL in Step 3.

- [ ] **Step 2: Create the CPA issue when no duplicate exists**

Run:

```powershell
$cpaBody = @'
## Summary

When a scheduler plugin delegates to CPA's built-in `fill-first` scheduler, CPA does not fall through from an unavailable highest auth-priority tier to usable accounts in lower tiers.

## Reproduction

1. Configure six Codex accounts.
2. Set two accounts to CPA auth priority `1`.
3. Set four accounts to CPA auth priority `0`.
4. Exhaust the five-hour quota of both priority-`1` accounts.
5. Keep all four priority-`0` accounts usable.
6. Delegate the scheduler pick to `fill-first`.

Observed result, with account identifiers redacted:

```text
fallback=fill-first
ordered_count=2
reason=fallback_fill_first
unavailable_summary=<high-a>:five_hour_exhausted:five_hour_exhausted;<high-b>:five_hour_exhausted:five_hour_exhausted
```

## Expected

The built-in scheduler should continue to lower priority tiers until it finds a usable account or exhausts all candidates.

## Actual

Only the two exhausted highest-priority accounts participate in fallback. Four usable lower-priority accounts are ignored.

## Environment

- CLIProxyAPI plugin SDK: v7.2.42
- Provider: Codex
- Built-in strategy: fill-first
'@
$cpaIssueUrl = gh issue create -R router-for-me/CLIProxyAPI --title 'Built-in scheduler does not fall through exhausted auth priority tiers' --body $cpaBody
$cpaIssueUrl
```

Expected: a new CPA issue URL.

- [ ] **Step 3: Create the linked plugin issue**

Run in the same PowerShell session:

```powershell
$pluginBody = @"
## Upstream dependency

CPA issue: $cpaIssueUrl

## v0.1.6 mitigation

- Admit only accounts in the maximum CPA priority tier observed in scheduler candidates.
- Do not load, refresh, display, or schedule lower CPA tiers.
- Recommend identical CPA priorities for every managed Codex account, preferably `0`.
- Add plugin-owned `scheduler_priority`, defaulting to `0`.
- Never inherit from or write priority to CPA.
- Fall through exhausted plugin priority tiers before built-in fallback.
- Prevent repeated refreshes when a successful refresh has already consumed a reset timestamp.

## Acceptance criteria

- [ ] Mixed CPA tiers never coexist in active plugin state.
- [ ] Lower CPA tiers are excluded from every refresh and management path.
- [ ] Plugin priority persists and is editable.
- [ ] Exhausted internal tiers fall through.
- [ ] Reset-trigger refresh cannot loop on an old timestamp.
- [ ] v0.1.6 is released and verified.

Close this issue after release. Leave a note that CPA priority integration may be reconsidered only after the upstream fix is released and independently verified.
"@
$pluginIssueUrl = gh issue create -R JefferyZhang2019/cpa-plugin-codex-quota-scheduler --title 'Isolate CPA priority and add plugin-owned account priority' --body $pluginBody
$pluginIssueUrl
```

Expected: the plugin issue links the CPA issue.

---

### Task 2: Add the CPA admission state

**Files:**
- Modify: `models.go`
- Modify: `scheduler.go`
- Modify: `state.go`
- Test: `scheduler_test.go`
- Test: `state_test.go`

**Interfaces:**
- Produces: `CPAAdmissionState`, `HighestPriorityCodexAdmission`, `ReplaceCPAAdmission`, `CPAAdmission`, `IsAuthAdmitted`, and `AdmittedCPAPriority`.

- [ ] **Step 1: Write failing maximum-tier extraction tests**

Add to `scheduler_test.go`:

```go
func TestHighestPriorityCodexAdmissionKeepsOnlyMaximumTier(t *testing.T) {
	req := pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high-a", Provider: "codex", Priority: 1},
		{ID: "low", Provider: "codex", Priority: 0},
		{ID: "high-b", Provider: "codex", Priority: 1},
		{ID: "other", Provider: "openai", Priority: 99},
	}}
	admission, ok := HighestPriorityCodexAdmission(req)
	if !ok || !admission.Observed || admission.Priority != 1 || len(admission.AuthIDs) != 2 {
		t.Fatalf("admission = %#v, ok=%t", admission, ok)
	}
	if _, ok := admission.AuthIDs["low"]; ok {
		t.Fatal("low CPA tier was admitted")
	}
}

func TestHighestPriorityCodexAdmissionRejectsNoCodexCandidates(t *testing.T) {
	admission, ok := HighestPriorityCodexAdmission(pluginapi.SchedulerPickRequest{
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "other", Provider: "openai", Priority: 9}},
	})
	if ok || admission.Observed {
		t.Fatalf("admission = %#v, ok=%t", admission, ok)
	}
}
```

- [ ] **Step 2: Verify RED**

Run:

```powershell
go test ./... -run TestHighestPriorityCodexAdmission -count=1
```

Expected: build failure because the admission type and helper do not exist.

- [ ] **Step 3: Implement the value and extraction helper**

Add to `models.go`:

```go
type CPAAdmissionState struct {
	Observed bool
	Priority int
	AuthIDs  map[string]struct{}
}
```

Add to `scheduler.go`:

```go
func HighestPriorityCodexAdmission(req pluginapi.SchedulerPickRequest) (CPAAdmissionState, bool) {
	admission := CPAAdmissionState{AuthIDs: make(map[string]struct{})}
	for _, candidate := range req.Candidates {
		if candidate.ID == "" || candidate.Provider != "codex" {
			continue
		}
		if !admission.Observed || candidate.Priority > admission.Priority {
			admission.Observed = true
			admission.Priority = candidate.Priority
			clear(admission.AuthIDs)
		}
		if candidate.Priority == admission.Priority {
			admission.AuthIDs[candidate.ID] = struct{}{}
		}
	}
	if !admission.Observed {
		admission.AuthIDs = nil
		return admission, false
	}
	return admission, true
}
```

Run the same test and expect PASS.

- [ ] **Step 4: Write failing state replacement tests**

Add to `state_test.go`:

```go
func TestReplaceCPAAdmissionPrunesExcludedAccounts(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "high", Provider: "codex", Priority: 1})
	store.UpsertQuota(AccountState{AuthID: "low", Provider: "codex", Priority: 0})
	store.ReplaceCPAAdmission(CPAAdmissionState{
		Observed: true,
		Priority: 1,
		AuthIDs:  map[string]struct{}{"high": {}},
	})
	snapshot := store.Snapshot(time.Now())
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].AuthID != "high" {
		t.Fatalf("accounts = %#v", snapshot.Accounts)
	}
	if store.IsAuthAdmitted("low") || !store.IsAuthAdmitted("high") {
		t.Fatalf("admission = %#v", store.CPAAdmission())
	}
}

func TestReplaceCPAAdmissionReplacesOldTier(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"old": {}}})
	store.UpsertQuota(AccountState{AuthID: "old", Provider: "codex", Priority: 1})
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 2, AuthIDs: map[string]struct{}{"new": {}}})
	if store.IsAuthAdmitted("old") || !store.IsAuthAdmitted("new") || len(store.Snapshot(time.Now()).Accounts) != 0 {
		t.Fatalf("state not replaced: %#v", store.Snapshot(time.Now()))
	}
}
```

- [ ] **Step 5: Verify RED and implement state methods**

Run:

```powershell
go test ./... -run TestReplaceCPAAdmission -count=1
```

Expected: build failure for missing state methods.

Add `cpaAdmission CPAAdmissionState` to `PluginState`, add `CPAAdmission CPAAdmissionState` to `StateSnapshot`, and implement:

```go
func cloneCPAAdmission(value CPAAdmissionState) CPAAdmissionState {
	cloned := CPAAdmissionState{Observed: value.Observed, Priority: value.Priority}
	if len(value.AuthIDs) > 0 {
		cloned.AuthIDs = make(map[string]struct{}, len(value.AuthIDs))
		for authID := range value.AuthIDs {
			cloned.AuthIDs[authID] = struct{}{}
		}
	}
	return cloned
}

func (s *PluginState) ReplaceCPAAdmission(value CPAAdmissionState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cpaAdmission = cloneCPAAdmission(value)
	if !value.Observed {
		return
	}
	for key, account := range s.accounts {
		if _, ok := value.AuthIDs[account.AuthID]; !ok {
			delete(s.accounts, key)
		}
	}
}

func (s *PluginState) CPAAdmission() CPAAdmissionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneCPAAdmission(s.cpaAdmission)
}

func (s *PluginState) IsAuthAdmitted(authID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.cpaAdmission.Observed || authID == "" {
		return false
	}
	_, ok := s.cpaAdmission.AuthIDs[authID]
	return ok
}

func (s *PluginState) AdmittedCPAPriority(authID string) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.cpaAdmission.Observed || authID == "" {
		return 0, false
	}
	_, ok := s.cpaAdmission.AuthIDs[authID]
	return s.cpaAdmission.Priority, ok
}
```

Expose a clone in `Snapshot`. Run admission tests and `go test ./... -count=1`; expect PASS.

- [ ] **Step 6: Commit**

```powershell
git add models.go scheduler.go scheduler_test.go state.go state_test.go
git commit -m "feat: track active CPA priority tier"
```

---

### Task 3: Constrain scheduler dispatch and every refresh entry point

**Files:**
- Modify: `dispatch.go`
- Modify: `refresh.go`
- Modify: `management.go`
- Test: `dispatch_test.go`
- Test: `refresh_test.go`
- Test: `management_test.go`

**Interfaces:**
- Consumes: admission methods from Task 2.
- Produces: no refresh or manual action can load an unadmitted account.

- [ ] **Step 1: Write failing dispatch and refresh tests**

Add tests proving:

```go
func TestSchedulerPickReplacesAdmissionBeforeRefresh(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	refresher := NewQuotaRefresher(&fakeHostClient{}, store, time.Now)
	withGlobalRefresherForTest(t, store, refresher)
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Provider: "codex", Priority: 1},
		{ID: "low", Provider: "codex", Priority: 0},
	}})
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatal(err)
	}
	admission := store.CPAAdmission()
	if admission.Priority != 1 || len(admission.AuthIDs) != 1 {
		t.Fatalf("admission = %#v", admission)
	}
}

func TestRefreshOnceDoesNothingBeforeAdmission(t *testing.T) {
	host := &fakeHostClient{authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", Provider: "codex"}}}
	refresher := NewQuotaRefresher(host, NewPluginState(DefaultConfig()), time.Now)
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatal(err)
	}
	if host.listCallCount() != 0 {
		t.Fatalf("ListAuths calls = %d", host.listCallCount())
	}
}

func TestRefreshOneRejectsOutsideAdmission(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"high": {}}})
	err := NewQuotaRefresher(&fakeHostClient{}, store, time.Now).RefreshOneAuthID("low")
	if err == nil || !strings.Contains(err.Error(), "outside the active CPA priority tier") {
		t.Fatalf("error = %v", err)
	}
}
```

Run these tests; expect failures.

- [ ] **Step 2: Observe admission in `handleSchedulerPick`**

Before recording activity or waking the refresher:

```go
if admission, ok := HighestPriorityCodexAdmission(req); ok {
	globalState.ReplaceCPAAdmission(admission)
	globalState.RecordLog("info", "scheduler.cpa_admission_updated", "CPA priority admission updated", map[string]any{
		"cpa_priority":  admission.Priority,
		"admitted_count": len(admission.AuthIDs),
		"excluded_count": codexCandidateCount(req) - len(admission.AuthIDs),
	}, now)
}
```

Add a deduplicating `codexCandidateCount` helper in `scheduler.go`.

- [ ] **Step 3: Gate refresh operations**

In `refresh.go`:

- Return before `ListAuths` from `RefreshOnce` and `RefreshDueOnce` when admission is unobserved.
- Replace `activePriorityCandidateIDs` with the stored admission set.
- Defensively call `ReplaceCPAAdmission` in `RefreshDueCandidatesOnce` when the request contains Codex candidates.
- Filter `ListAuths` results with `state.IsAuthAdmitted(auth.ID)`.
- Reject `RefreshOneAuthID` before listing auths when the ID is not admitted.
- At the start of `refreshAuth`, require `AdmittedCPAPriority(auth.ID)`; return when false and set `account.Priority` to the returned CPA priority.
- Count only admitted Codex auths in `recordAuthScan`.

Use:

```go
func filterAdmittedAuths(auths []pluginapi.HostAuthFileEntry, state *PluginState) []pluginapi.HostAuthFileEntry {
	filtered := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	for _, auth := range auths {
		if isRefreshEligible(auth) && state.IsAuthAdmitted(auth.ID) {
			filtered = append(filtered, auth)
		}
	}
	return filtered
}
```

- [ ] **Step 4: Reject excluded management refreshes**

Add a failing test expecting HTTP 409 for `/refresh/account` with an excluded ID. Then add before `triggerRefreshOneSoon`:

```go
if !store.IsAuthAdmitted(authID) {
	return jsonManagementResponse(http.StatusConflict, map[string]string{
		"error": fmt.Sprintf("auth %s is outside the active CPA priority tier", authID),
	})
}
```

- [ ] **Step 5: Update startup behavior and verify**

Change the existing startup-refresh test: `refresh_on_startup` may start the worker, but it must not scan auths until a scheduler request establishes admission.

Run:

```powershell
gofmt -w dispatch.go dispatch_test.go refresh.go refresh_test.go management.go management_test.go scheduler.go
go test ./... -count=1
```

Expected: PASS and no lower-tier refresh.

- [ ] **Step 6: Commit**

```powershell
git add dispatch.go dispatch_test.go refresh.go refresh_test.go management.go management_test.go scheduler.go
git commit -m "fix: isolate refreshes to active CPA tier"
```

---

### Task 4: Make reset-trigger refreshes one-shot

**Files:**
- Modify: `state.go`
- Test: `state_test.go`
- Test: `refresh_test.go`

**Interfaces:**
- Produces: `refreshTriggerPending(lastSuccessAt, triggerAt time.Time) bool`.
- Prevents: an old five-hour, long-window, or temporary reset timestamp from scheduling another immediate refresh after success.

- [ ] **Step 1: Write failing state tests**

Add:

```go
func TestAccountRefreshDueIgnoresResetConsumedBySuccess(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = 30 * time.Minute
	account := weeklyAccount("auth-1", 1, now.Add(24*time.Hour), true)
	account.LastSuccessAt = now.Add(-time.Minute)
	account.Quota.FiveHour.ResetAt = now.Add(-10 * time.Minute)
	due, reason := accountRefreshDue(account, cfg, now)
	if due || reason != "" {
		t.Fatalf("due=%t reason=%q", due, reason)
	}
}

func TestNextRefreshDueAtIgnoresConsumedReset(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = 30 * time.Minute
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	account := weeklyAccount("auth-1", 1, now.Add(24*time.Hour), true)
	account.LastSuccessAt = now.Add(-time.Minute)
	account.Quota.FiveHour.ResetAt = now.Add(-10 * time.Minute)
	store.UpsertQuota(account)
	want := account.LastSuccessAt.Add(cfg.QuotaRefreshInterval)
	if got := store.NextRefreshDueAt(now); !got.Equal(want) {
		t.Fatalf("next=%s want=%s", got, want)
	}
}
```

Run; expect failures showing the reset remains immediately due.

- [ ] **Step 2: Implement consumed-trigger detection**

Add:

```go
func refreshTriggerPending(lastSuccessAt, triggerAt time.Time) bool {
	return !triggerAt.IsZero() && (lastSuccessAt.IsZero() || lastSuccessAt.Before(triggerAt))
}
```

Change `resetDue` to:

```go
func resetDue(window *QuotaWindow, delay time.Duration, lastSuccessAt, now time.Time) bool {
	if window == nil || window.ResetAt.IsZero() {
		return false
	}
	triggerAt := window.ResetAt.Add(delay)
	return refreshTriggerPending(lastSuccessAt, triggerAt) && !triggerAt.After(now)
}
```

Pass `account.LastSuccessAt` from `accountRefreshDue`. Apply the same check to temporary resets and to each reset trigger considered by `NextRefreshDueAt`.

- [ ] **Step 3: Write the two-second regression test**

Add to `refresh_test.go` a fake-clock test that refreshes an account whose upstream response repeats an already-past reset timestamp, advances the clock by two seconds, calls `RefreshDueOnce` again, and asserts the HTTP call count is unchanged.

Run:

```powershell
go test ./... -run 'TestAccountRefreshDueIgnoresResetConsumedBySuccess|TestNextRefreshDueAtIgnoresConsumedReset|TestRefreshDueOnceDoesNotRepeatSuccessfulRefreshForObservedPastReset' -count=1
```

Expected: PASS.

- [ ] **Step 4: Run full tests and commit**

```powershell
gofmt -w state.go state_test.go refresh_test.go
go test ./... -count=1
git add state.go state_test.go refresh_test.go
git commit -m "fix: consume reset refresh triggers once"
```

---

### Task 5: Add plugin-owned priority and internal fallback

**Files:**
- Modify: `models.go`
- Modify: `scheduler.go`
- Modify: `dispatch.go`
- Modify: `management.go`
- Test: `scheduler_test.go`
- Test: `dispatch_test.go`
- Test: `management_test.go`

**Interfaces:**
- Produces: `AccountAnnotation.SchedulerPriority`, `ScheduledAccount.CPAPriority`, and `ScheduledAccount.SchedulerPriority`.

- [ ] **Step 1: Write failing scheduling tests**

Add tests where:

1. All candidates have CPA priority `0`; plugin priority `10` wins over plugin priority `0`.
2. Two plugin-priority-`1` accounts are five-hour exhausted and a plugin-priority-`0` account is selected.
3. Mixed CPA priority candidates produce an ordered list containing only the maximum CPA tier.
4. Missing plugin priority behaves as zero.

Core assertion:

```go
decision := PickCodexAccount(req, snapshot, now)
if decision.AuthID != "available-low-plugin-tier" || decision.DelegateBuiltin != "" {
	t.Fatalf("decision = %#v", decision)
}
```

Run; expect build and behavior failures.

- [ ] **Step 2: Add the annotation field**

Change `AccountAnnotation`:

```go
type AccountAnnotation struct {
	Alias             string   `json:"alias,omitempty" yaml:"alias,omitempty"`
	Notes             string   `json:"notes,omitempty" yaml:"notes,omitempty"`
	Tags              []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	GroupID           string   `json:"group_id,omitempty" yaml:"group_id,omitempty"`
	SchedulerPriority int      `json:"scheduler_priority,omitempty" yaml:"scheduler_priority,omitempty"`
}
```

- [ ] **Step 3: Separate CPA and plugin priorities**

Change `ScheduledAccount` to contain:

```go
CPAPriority       int
SchedulerPriority int
```

`BuildOrderedAccounts` must:

- call `HighestPriorityCodexAdmission`;
- skip candidates outside `admission.AuthIDs`;
- populate CPA priority from the request candidate;
- populate scheduler priority from `account.Annotation.SchedulerPriority`;
- sort scheduler priority descending before queue status and existing within-tier rules.

The first sort clause is:

```go
if left.SchedulerPriority != right.SchedulerPriority {
	return left.SchedulerPriority > right.SchedulerPriority
}
```

- [ ] **Step 4: Scan all plugin tiers before fallback**

Replace the active-priority break in `PickCodexAccount` with a loop returning the first available account in the already sorted admitted list. Apply the same behavior to `nextStatusAuthID`.

- [ ] **Step 5: Separate diagnostics**

Selection logs must emit:

```go
fields["selected_cpa_priority"] = selected.CPAPriority
fields["selected_scheduler_priority"] = selected.SchedulerPriority
```

Update tests to assert both.

- [ ] **Step 6: Verify and commit**

```powershell
gofmt -w models.go scheduler.go scheduler_test.go dispatch.go dispatch_test.go management.go management_test.go
go test ./... -count=1
git add models.go scheduler.go scheduler_test.go dispatch.go dispatch_test.go management.go management_test.go
git commit -m "feat: add plugin-owned account priority"
```

---

### Task 6: Persist and edit plugin priority

**Files:**
- Modify: `annotations_test.go`
- Modify: `disk_state_test.go`
- Modify: `management.go`
- Modify: `management_test.go`

**Interfaces:**
- Produces: annotation patch field `scheduler_priority`, status JSON field, and management UI editor.

- [ ] **Step 1: Add persistence regression tests**

Update annotation and disk-state round-trip tests to store priority `7`, load it as `7`, and confirm absent fields load as `0`.

Because the model field from Task 5 makes JSON persistence work automatically, verify test sensitivity by temporarily removing the field, observing failure, then restoring it before continuing.

- [ ] **Step 2: Write failing patch and status tests**

Add an annotation PATCH test with body:

```json
{"auth_id":"auth-1","scheduler_priority":8}
```

Assert stored priority is `8`. Add a status test asserting `cpa_priority: 3` and `scheduler_priority: 8` are distinct.

Run; expect patch/status failures.

- [ ] **Step 3: Implement management fields**

Add:

```go
SchedulerPriority *int `json:"scheduler_priority"`
```

to `annotationPatch`, assign it when non-nil, add:

```go
SchedulerPriority int `json:"scheduler_priority"`
```

to `StatusAccount`, and populate it from `ScheduledAccount.SchedulerPriority`.

- [ ] **Step 4: Add the UI editor and badges**

Add a number input:

```html
<label class="field"><span data-i18n="account.schedulerPriority">插件优先级</span><input id="editSchedulerPriority" type="number" step="1" value="0"></label>
```

Populate it in `openEdit`, send it in `saveAccountModal`, and render separate CPA and plugin priority badges. Add English and Chinese translations.

Extend page marker tests to require:

```text
id="editSchedulerPriority"
account.schedulerPriority
scheduler_priority
Plugin priority
插件优先级
```

- [ ] **Step 5: Verify and commit**

```powershell
gofmt -w annotations_test.go disk_state_test.go management.go management_test.go
go test ./... -count=1
git add annotations_test.go disk_state_test.go management.go management_test.go
git commit -m "feat(ui): manage plugin account priority"
```

---

### Task 7: Add incident integration coverage and v0.1.6 guidance

**Files:**
- Modify: `integration_test.go`
- Modify: `README.md`
- Modify: `Makefile`

**Interfaces:**
- Produces: six-account regression coverage and exact upgrade instructions.

- [ ] **Step 1: Add six-account integration tests**

Test A: six accounts all use CPA priority `0`; two plugin-priority-`1` accounts are exhausted; four plugin-priority-`0` accounts are usable; the first usable lower internal tier is selected without fallback.

Test B: the original mixed CPA priorities remain `1/0`; only the two CPA-priority-`1` accounts appear in `decision.Ordered`; the lower CPA tier is intentionally excluded.

Run both tests and expect PASS only after Tasks 2-6.

- [ ] **Step 2: Update README**

Document exactly:

- v0.1.6 loads only the maximum observed CPA priority tier.
- Users must set all managed Codex accounts to the same CPA priority; `0` is recommended.
- Lower CPA tiers are not loaded, refreshed, displayed, or scheduled.
- Plugin priority is independent, defaults to `0`, and falls through exhausted internal tiers.
- Plugin priority never reads from or writes to CPA.
- Reset timestamps already consumed by a successful refresh cannot create a two-second refresh loop.
- Link both remote issues.

- [ ] **Step 3: Update version references**

Set:

```make
VERSION ?= 0.1.6
```

Update current package, tag, checksum, and asset examples to `0.1.6`. Preserve v0.1.5 as historical release information.

- [ ] **Step 4: Verify and commit**

```powershell
gofmt -w integration_test.go
go test ./... -count=1
go vet ./...
git diff --check
git add integration_test.go README.md Makefile
git commit -m "docs: prepare v0.1.6 priority isolation"
```

---

### Task 8: Verify, publish, and close the plugin issue

**Files:** No additional source changes expected.

**Interfaces:**
- Produces: pushed main, v0.1.6 tag, GitHub release, verified assets, and a closed plugin issue.

- [ ] **Step 1: Run the fresh completion gate**

```powershell
gofmt -l *.go
go test ./... -count=1
go vet ./...
git diff --check
git status --short --branch
```

Expected: no formatting output, zero test/vet failures, no whitespace errors, and a clean worktree.

- [ ] **Step 2: Build locally**

```powershell
./build.ps1
```

Expected: `dist/codex-quota-scheduler.dll` exists. If the C compiler is unavailable, report the exact limitation and do not claim local build success.

- [ ] **Step 3: Review and push without force**

```powershell
git log --oneline origin/main..HEAD
git diff --stat origin/main..HEAD
git push origin main
git tag -a v0.1.6 -m "v0.1.6"
git push origin v0.1.6
```

Expected: only approved v0.1.5, design/plan, and v0.1.6 work is pushed.

- [ ] **Step 4: Verify GitHub Actions and release assets**

```powershell
$runId = gh run list -R JefferyZhang2019/cpa-plugin-codex-quota-scheduler --workflow Build --branch v0.1.6 --limit 1 --json databaseId --jq '.[0].databaseId'
gh run watch $runId -R JefferyZhang2019/cpa-plugin-codex-quota-scheduler --exit-status
gh release view v0.1.6 -R JefferyZhang2019/cpa-plugin-codex-quota-scheduler --json url,assets
```

Expected: all platform build jobs and the release job pass; seven platform ZIPs plus `checksums.txt` are present.

- [ ] **Step 5: Close the plugin issue with the upstream note**

```powershell
$pluginIssue = gh issue list -R JefferyZhang2019/cpa-plugin-codex-quota-scheduler --state open --search 'Isolate CPA priority and add plugin-owned account priority in:title' --json number --jq '.[0].number'
$cpaIssueUrl = gh issue list -R router-for-me/CLIProxyAPI --state all --search 'Built-in scheduler does not fall through exhausted auth priority tiers in:title' --json url --jq '.[0].url'
$note = @"
Released in v0.1.6.

The plugin now loads only the highest observed CPA priority tier, maintains an independent plugin priority, falls through exhausted internal tiers, and prevents repeated refreshes from consumed reset timestamps. Configure all CPA Codex priorities identically, preferably `0`.

Upstream tracking: $cpaIssueUrl

CPA priority integration may be reconsidered only after the upstream fix is released and independently verified.
"@
gh issue comment $pluginIssue -R JefferyZhang2019/cpa-plugin-codex-quota-scheduler --body $note
gh issue close $pluginIssue -R JefferyZhang2019/cpa-plugin-codex-quota-scheduler --reason completed
```

Expected: the plugin issue is closed; the CPA issue remains available for upstream tracking.

- [ ] **Step 6: Report evidence**

Return both issue URLs, the release URL, fresh tests/vet/build results, the Actions run result, and the operational warning to set every CPA Codex account to the same priority, preferably `0`.
