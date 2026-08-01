# v0.2.1 Lazy-Window Port And Release Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port PR #3's baseline migration, confirmed-window re-arming, and dormant read-only observations onto the hardened local Probe architecture, then publish v0.2.1 and update the Plugin Store.

**Architecture:** Extend the existing `SuspectedLazy` path instead of merging PR #3. Known-length baselines receive bounded read-only deadlines; strict zero-usage evidence rebases them into the existing single-lease activation sequence. Publish only after independent review, fresh local gates, main CI, and tag CI pass.

**Tech Stack:** Go 1.26, CPA plugin ABI v7, JSON durable state, embedded bilingual Management UI, PowerShell S7 gates, GitHub Actions/CLI, CLIProxyAPI Plugin Store registry.

## Global Constraints

- Work in `.worktrees/superpowers-refactor-spec-plan` on `codex/fix-first-observation-probe` until integration.
- Preserve the user's dirty main checkout (`docs/fable-spec/refactor-decision-spec.md`, `.superpowers/`) and the worktree's untracked `docs/superpowers/plans/2026-07-13-s7-final-review-fixes.md`.
- Keep `enable_reset_probe` as the only opt-in gate; add no setting.
- Observation interval is exactly `max(quota_refresh_interval, 30m)`.
- Missing `used_percent` is never zero and never authorizes compact send.
- Preserve zero `PendingCheck` deadline, roster/credential gates, coordinator lease, WAL/fence/suppression, verify-first recovery, atomic exact-attempt completion, instance-local persistence, coalesced rerun, and redacted lifecycle logs.
- Compact send requires valid snapshot, explicit zero usage, authoritative reset/duration, reset within three minutes of `observed_at + duration`, and a newly observed reset or one-time v0.2.0 migration.
- Do not mutate GitHub, PR #3, tags, releases, or Plugin Store before local review and all local gates pass.
- Publish exactly `0.2.1` with annotated tag `v0.2.1`.

---

### Task 1: Strict Evidence And v0.2.0 Baseline Migration

**Files:**
- Modify: `probe_classify.go`, `probe_runtime.go`, `docs/deviations.md`
- Test: `probe_classify_test.go`, `probe_runtime_test.go`

**Interfaces:**
- Consumes: `ProbeBaseline`, `QuotaWindow`, `probeWindowDuration`, `resetProbeCloseThreshold`, `SuspectedLazy`.
- Produces: `usageActivated(old, new float64) bool`, `looksLikeStrictLazyObservation(observedAt time.Time, window QuotaWindow, length time.Duration) bool`, and one-time unknown-length migration.

- [ ] **Step 1: Write classifier RED tests**

Add:

```go
func TestUsageActivationRequiresConsumedToZero(t *testing.T) {
	for _, tc := range []struct{ old, new float64; want bool }{
		{80, 0, true}, {0, 0, false}, {80, 1, false},
	} {
		if got := usageActivated(tc.old, tc.new); got != tc.want {
			t.Fatalf("usageActivated(%v,%v)=%v want %v", tc.old, tc.new, got, tc.want)
		}
	}
}
```

Table-test strict FiveHour, Weekly, explicit-duration Monthly, missing usage, non-zero usage, missing Monthly duration, and reset outside three minutes.

- [ ] **Step 2: Verify RED**

Run `go test . -run 'TestUsageActivationRequiresConsumedToZero|TestStrictLazyObservation' -count=1`.

Expected: `0 -> 0` is incorrectly true and the helper is absent.

- [ ] **Step 3: Implement strict helpers**

```go
func usageActivated(old, new float64) bool { return old > 0 && new == 0 }

func looksLikeStrictLazyObservation(observedAt time.Time, window QuotaWindow, length time.Duration) bool {
	if window.UsedPercent == nil || *window.UsedPercent != 0 || observedAt.IsZero() || window.ResetAt.IsZero() || length <= 0 {
		return false
	}
	return absDuration(window.ResetAt.Sub(observedAt.Add(length))) <= resetProbeCloseThreshold
}
```

Use existing duration rules; never synthesize Monthly duration.

- [ ] **Step 4: Write migration/restart RED tests**

Seed `ProbeWaitingReset` with `ResetProbeBaseline(reset, 0, 0)`, provide authoritative zero-usage quota with known length, bootstrap, and require filled `WindowLength`, `SuspectedLazy`, `PendingCheck`, and zero deadline. Add a known-length durable restart with no quota cache.

Run `go test . -run 'TestProbeBootstrapMigratesV020LazyBaselineImmediately|TestProbeBootstrapLoadsKnownLengthBaselineWithoutQuotaCache' -count=1`.

Expected: FAIL because existing windows are skipped.

- [ ] **Step 5: Implement one-time migration**

For an existing reset baseline whose length is zero, fill it only from the current authoritative observation. If strict lazy evidence matches, rebase reset/usage, set `SuspectedLazy`, enter `PendingCheck`, and clear deadline. Persist through the existing state store.

- [ ] **Step 6: Trace and commit**

Add `DEV-013` to `docs/deviations.md` with exact tests and `Approved 2026-08-01`.

```powershell
go test . -run 'TestUsageActivation|TestStrictLazyObservation|TestProbeBootstrapMigratesV020|TestProbeBootstrapLoadsKnownLength|TestFirstObserved' -count=1
go test ./... -count=1 -timeout 300s
git add probe_classify.go probe_classify_test.go probe_runtime.go probe_runtime_test.go docs/deviations.md
git commit -m "fix(probe): migrate lazy reset baselines"
```

---

### Task 2: Dormant Observations And Confirmed Re-Arming

**Files:**
- Modify: `probe_state.go`, `probe_runtime.go`, `docs/deviations.md`
- Test: `probe_state_test.go`, `probe_runtime_test.go`

**Interfaces:**
- Consumes: Task 1 strict helper, `ProbeEvent`, `deadlineFor`, bootstrap/reconciliation, `RunProbeDueOnce`, atomic terminal completion.
- Produces: `probeObservationInterval()`, observation-aware deadlines, external-lazy rebasing, and `Confirmed` re-arming.

- [ ] **Step 1: Write deadline RED tests**

Extend `ProbeEvent` with `ObservationInterval time.Duration`. Test known 5-hour reset at `now+5h` schedules `now+30m`, close reset maturity wins, and configured `10m` floors to `30m` while `45m` remains `45m`.

Run `go test . -run 'TestKnownResetDeadlineIncludesBoundedObservation|TestProbeObservationIntervalHasThirtyMinuteFloor' -count=1`.

Expected: compile/behavior FAIL.

- [ ] **Step 2: Implement observation scheduling**

```go
func (r *QuotaRefresher) probeObservationInterval() time.Duration {
	interval := NormalizeConfig(r.state.Config()).QuotaRefreshInterval
	if interval < probeUnknownResetRecheck { return probeUnknownResetRecheck }
	return interval
}
```

Change `deadlineFor` to `deadlineFor(base ProbeBaseline, now time.Time, observationInterval time.Duration) time.Time`. For known reset length return the earlier reset-maturity or observation deadline. Pass zero where runtime configuration is intentionally unavailable. During bootstrap, advance durable known-length `WaitingReset` deadlines even with no quota cache.

- [ ] **Step 3: Write external-reset RED tests**

Prove unchanged reset/usage reschedules with zero POST; shifted reset plus strict zero-usage signature sends one POST; normal refresh stays Dormant; missing evidence sends none.

Run `go test . -run 'TestUnchangedWindowObservationReschedulesWithoutProbe|TestExternalResetObservationSendsOnce|TestProductionProbeDetectsExternalResetWhileNormalRefreshDormant' -count=1`.

Expected: shifted reset is generically classified as activated and periodic observation is absent.

- [ ] **Step 4: Rebase strict precheck through `SuspectedLazy`**

Only during precheck, when strict evidence is valid and reset shifted beyond skew or baseline was migrated, clone the baseline, replace reset/usage, set `SuspectedLazy`, then call `ClassifyProbeWindow`. Verify must keep the generic classifier. Pass `ObservationInterval` so unchanged windows schedule their next read.

- [ ] **Step 5: Write and implement Confirmed re-arm tests**

Test later authoritative observation of `Confirmed`: valid ordinary evidence -> fresh baseline and bounded `WaitingReset`; strict lazy evidence -> fresh `SuspectedLazy` and zero-deadline `PendingCheck` plus production wake; invalid/missing evidence -> remains `Confirmed`.

Implement re-arm in authoritative observation reconciliation/bootstrap, scoped to one instance. Never replace the entire persisted `ProbeWindows` map.

- [ ] **Step 6: Preserve overlap/restart safety**

Extend the two-instance barrier: hold A, re-arm B from external lazy evidence after A snapshots bindings, release A, require exactly one A/B POST, durable B state, restart preservation, original attempts/fences, and no duplicate A.

- [ ] **Step 7: Trace, verify, and commit**

Add `DEV-014` with exact dormant-observation tests and `Approved 2026-08-01`.

```powershell
go test . -run 'TestKnownReset|TestProbeObservation|TestUnchangedWindowObservation|TestExternalReset|TestReconcileConfirmed|TestProbeRefreshDuringActiveRun' -count=1
go test -race . -run 'TestExternalReset|TestReconcileConfirmed|TestProbeRefreshDuringActiveRun' -count=1
go test ./... -count=1 -timeout 300s
git add probe_state.go probe_state_test.go probe_runtime.go probe_runtime_test.go docs/deviations.md
git commit -m "feat(probe): observe and rearm lazy windows"
```

---

### Task 3: Management Disclosure And v0.2.1 Metadata

**Files:**
- Modify: `management.go`, `README.md`, `README.zh-CN.md`, `config.go`, `Makefile`
- Test: `management_test.go`, `config_test.go`

**Interfaces:**
- Consumes: `enable_reset_probe`, `pluginVersion`, Management translations, packaging variables.
- Produces: bilingual opt-in disclosure and consistent version `0.2.1`.

- [ ] **Step 1: Write Management-copy RED test**

Assert both languages state: read-only checks may run while normal refresh is dormant; interval follows quota refresh with 30-minute minimum; compact request is conditional and may consume small quota.

Run `go test . -run 'TestManagement.*ResetProbe.*Copy' -count=1` and expect FAIL.

- [ ] **Step 2: Update embedded copy**

Use this meaning:

```text
EN: Probe performs read-only checks at the quota refresh interval with a
30-minute minimum, even while normal refresh is dormant, and sends one tiny
request only after detecting a lazy reset window.

ZH-CN: 即使普通刷新处于休眠状态，Probe 也会按额度刷新间隔执行只读检查，最短
30 分钟；只有检测到延迟启动的重置窗口时，才发送一次极小请求。
```

- [ ] **Step 3: Write version RED tests**

Expect registration `0.2.1`; statically require `config.go`, `Makefile`, both README release headings, build examples, and tag examples to agree.

Run `go test . -run 'TestPluginRegistrationUsesV021SourceVersion|TestReleaseVersionMetadataConsistent' -count=1` and expect FAIL.

- [ ] **Step 4: Update release metadata/docs**

Set `pluginVersion = "0.2.1"`, `VERSION ?= 0.2.1`, and add concise v0.2.1 English/Chinese sections for migrated/fresh activation, dormant observation, re-arm, and crash/concurrency safety. Keep the broader v0.2.0 scheduler overview.

- [ ] **Step 5: Verify and commit**

```powershell
go test . -run 'TestManagement|TestPluginRegistration|TestReleaseVersion' -count=1
go test ./... -count=1 -timeout 300s
git add management.go management_test.go README.md README.zh-CN.md config.go config_test.go Makefile
git commit -m "chore: prepare v0.2.1 release"
```

---

### Task 4: Review And Local Release Gates

**Files:**
- Modify only Task 1-3 files if review finds a defect.
- Produce: `dist/codex-quota-scheduler.dll`, Windows AMD64 v0.2.1 ZIP/checksum.

- [ ] **Step 1: Independent review**

Review strict evidence, bounded false-positive risk, dormant zero-normal-refresh contract, atomic verify-first recovery, instance-local persistence/coalesced rerun, disclosure, and version consistency. Fix every Critical/Important finding with focused tests and re-review.

- [ ] **Step 2: Run fresh gates**

```powershell
go test ./... -count=1 -timeout 300s
go test -race ./... -count=1 -timeout 300s
go vet ./...
& .\scripts\check_refactor_gates.ps1 -Stage S7
& .\build.ps1
Get-Item .\dist\codex-quota-scheduler.dll | Select-Object FullName,Length,LastWriteTime
Get-FileHash .\dist\codex-quota-scheduler.dll -Algorithm SHA256
```

Expected: exit 0, no races, S7 49 exact Mock row owners, DLL metadata/hash recorded.

- [ ] **Step 3: Build local release package**

From Git Bash run `make clean && make package VERSION=0.2.1 GOOS=windows GOARCH=amd64`.

Expected: ZIP and `.sha256`; archive contains DLL and no generated header.

- [ ] **Step 4: Confirm integration precondition**

Run `git fetch origin`, `git merge-base --is-ancestor origin/main HEAD`, `git diff --check origin/main..HEAD`, and `git status --short`.

Expected: fast-forward is possible and only the protected untracked plan remains.

---

### Task 5: GitHub Release, PR #3 Closure, And Plugin Store

**Repositories:**
- Publish: `JefferyZhang2019/cpa-plugin-codex-quota-scheduler`
- Close: PR #3
- Store: `router-for-me/CLIProxyAPI-Plugins-Store`

- [ ] **Step 1: Fast-forward remote main without touching dirty local main**

Run `git fetch origin`, recheck ancestry, then `git push origin HEAD:main`.

Expected: fast-forward succeeds; never checkout/reset the user's main checkout.

- [ ] **Step 2: Wait for main Build workflow**

Resolve the run whose head SHA equals published HEAD and use `gh run watch <id> --repo JefferyZhang2019/cpa-plugin-codex-quota-scheduler --exit-status`.

Expected: Test/Vet and all seven platform builds pass. If not, do not tag; diagnose, fix, re-review, and repeat gates/push.

- [ ] **Step 3: Tag and verify release**

Create annotated `v0.2.1`, verify it points to tested remote main, push it, watch tag Build/Release, then run:

```powershell
gh release view v0.2.1 --repo JefferyZhang2019/cpa-plugin-codex-quota-scheduler --json isDraft,isPrerelease,tagName,assets,url
```

Expected: published stable release, seven platform ZIPs plus `checksums.txt`, seven checksum entries.

- [ ] **Step 4: Close PR #3 after release**

Post through the GitHub connector:

```text
Released in v0.2.1 through the locally hardened Probe architecture. The release
incorporates this contribution's v0.2.0 baseline migration, Confirmed-window
re-arming, and dormant read-only observation ideas while preserving atomic
attempt/window completion, verify-first recovery, instance-scoped persistence,
coalesced cross-account wakeups, and lifecycle logging. Credit: @Siriussee.
```

Close PR #3 without merging and include the release URL. Verify closed state.

- [ ] **Step 5: Create isolated Store branch**

In `CLIProxyAPI-Plugins-Store`, fetch upstream, verify `.worktrees` is ignored, and create `.worktrees/update-quota-scheduler-0.2.1` from `upstream/main` on `codex/update-quota-scheduler-0.2.1`. Change only the scheduler registry version `0.2.0 -> 0.2.1`.

- [ ] **Step 6: Validate and open Store PR**

Parse `registry.json`, run `git diff --check`, commit `chore: bump codex quota scheduler to 0.2.1`, push to the fork, and create an upstream PR. The body must include the release URL, stable status, seven ZIPs plus checksums, seven checksum entries, JSON parse success, and explain the registry version is the fallback.

- [ ] **Step 7: Verify Store state**

Confirm the Store PR patch changes exactly one line. If upstream merges during this task, verify visible Store version `0.2.1`; otherwise report the PR URL and the external maintainer-merge dependency.
