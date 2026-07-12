# Task 6 / S5 Report

## RED

`go test ./... -run TestSuiteScheduling -count=1` failed at compile time on the intentionally absent S5 surface: `AccountView`, `AvailabilityClass`, `ClassifyAccount`, snapshot publication, and trial registry APIs. This was recorded before production implementation.

## GREEN and exhaustive oracle

- `TestMockGroupB` independently implements the design classification table without calling production classification or ordering helpers.
- Single-instance matrix: exactly 1,440 vectors (`4 cache × 5 exhaustion × 2 auth × 3 circuit × 2 temporary × 3 trial × 2 priority`).
- Multi-instance matrix: exactly 774 comparisons (`3 × (6 + 36 + 216)`), covering N=1..3 and candidate superset/subset/empty-intersection relationships.
- Named coverage includes class-before-plugin-priority, authoritative-tier intersection, optimistic trial CAS, real-evidence release, 60-second pending retention, five-minute evidence budget, and real ABI atomic snapshot loading.

## Snapshot-only real ABI and static gate

- `handleSchedulerPick` decodes the ABI request and delegates to the immutable published `atomic.Pointer[SchedulerSnapshot]` path.
- Candidates only intersect the prepublished authoritative highest tier. They never replace CPA admission or create roster/account state.
- Opportunistic selection uses per-instance CAS and a nonblocking evidence-intent send.
- `go run ./scripts/refactor_gates/analyze_pick_path.go -root . -entry handleSchedulerPick` returned `[]`.
- `scripts/refactor_gates/s1-pick-path-baseline.json` is now `[]`.
- `./scripts/check_refactor_gates.ps1 -Stage S5` passed the exact `TestSchedulerPickABIPathSnapshotOnly` gate.

## Benchmark

Baseline: 1244 ns/op, 2312 B/op, 6 allocs/op.

S5 three runs: 958.5 / 973.2 / 1861 ns/op, 2696 B/op, 6 allocs/op. Allocation count is unchanged; bytes are +384 B/op; latency is noisy with two faster runs and one slower run.

## Verification

- `go test ./... -run TestSuiteScheduling -count=1` — PASS.
- `go test -race ./... -run 'Test(SchedulerPick|Trial)' -count=1` — PASS.
- `./scripts/check_refactor_gates.ps1 -Stage S5` — PASS.
- `go vet ./...` — PASS after legacy dispatch tests were rewritten for the S5 contract.
- Full `go test ./... -count=1` — PASS.

## Files and self-review

Added `selection.go`, `selection_test.go`, `trial.go`, `trial_test.go`, `scheduler_snapshot.go`, and `scheduler_snapshot_test.go`. Updated coordinator snapshot naming, ABI dispatch, authoritative-roster/refresh publication, reliable-writeback trial evidence, dispatch contract tests, and the S1 AST baseline.

No S6 Probe transition or S7 roster lifecycle behavior was added. Trial timeout progression is explicit through `Advance`/retry evidence rather than a periodic scheduler, preserving INV-41 (no automatic periodic re-admission).

## Concerns

No open S5 concern remains after the correction wave below.

## Review correction wave

The review findings were addressed test-first in a follow-up wave:

- Production snapshots now always carry the bounded 64-entry S5 evidence-intent channel. Successful Opportunistic CAS sends `{auth, instance, began_at}` nonblocking. The S5 consumer marks evidence pending and asynchronously calls the existing `RefreshOneSoon` normal-refresh/coordinator path. A full queue safely drops the intent; the trial remains subject to timeout/budget.
- Production refresh failures call `ObserveRetry`; reliable refresh writeback and usage/request evidence clear the trial. Selection calls event-driven `Advance` on real picks: pending evidence remains excluded at 60s, while 5m or the third retry forces TrialUnknown. No periodic re-admission scanner exists.
- TrialUnknown becomes dynamically eligible after `nextRetry`; real pick consults `TrialRegistry.State(now)` on every selection and does not trust the snapshot's trial bit. Backoff is exactly 1m, 2m, 5m, 10m, 15m, then capped at 15m.
- `AccountView` carries the real three-state circuit class. The 1,440 classification matrix therefore exercises Closed/Open/HalfOpen production data rather than a test-only dimension.
- The oracle is now independently table-driven. All 774 multi-instance relations assert selected auth, confidence class, fallback, trial start, evidence source, and reason. A priority-before-class mutant is explicitly killed by oracle expectations.
- The real ABI test encodes a plugin request, invokes `handleSchedulerPick`, decodes the response, proves authoritative-tier intersection, and proves candidates do not mutate roster admission. Concurrent alternating publications prove one immutable snapshot is observed per decision.
- All unreachable historical dispatch bodies were deleted. There are no skips or unreachable compatibility branches.
- Per-pick snapshot/map cloning was removed. Corrected real published-path benchmark: 403.9 / 418.1 / 1210 ns/op, 664 B/op, 6 allocs/op. This is materially below both the S1 baseline (2312 B/op) and the first S5 review build (2696 B/op).

Corrected verification: focused lifecycle/backoff/queue/evidence tests PASS; `TestSuiteScheduling` PASS; race `Test(SchedulerPick|Trial)` PASS; S5 static/ABI gate PASS with analyzer `[]`; full tests, vet, and diff check PASS.

The two earlier concerns are resolved: bytes/op no longer regress materially, and historical unreachable bodies have been removed.

## Final evidence-race correction

- After an Opportunistic CAS, a successful nonblocking queue send now synchronously marks that exact trial evidence-pending before `scheduler.pick` returns. A deterministic unconsumed-queue test advances to 60s and proves the trial remains held. The distinct queue-full test proves a dropped intent is not marked pending and transitions to TrialUnknown at 60s.
- `MarkEvidencePending` remains a CAS update of an existing record only. A deterministic evidence-before-mark test clears the record first, applies the late pending mark, and proves the trial is not recreated.
- The actual `handleUsageHandle` business path now emits `EvidenceRequestSuccess` for successful Codex usage records and `EvidenceUsageFeedback` only for detected quota-limit feedback. It resolves the correct instance by auth ID or auth index, clears the dynamic registry immediately, and the next encoded ABI pick observes eligibility without snapshot republishing. Probe verification remains isolated and does not clear business-request trials.
- Final focused queue-race/request-success tests, scheduling suite/oracle, ABI race/static S5 gate, full tests, vet, and diff check all pass (see final handoff verification).

## Quota-limit snapshot correction

Quota-limit (`usage_limit_reached`) feedback now republishes the scheduler snapshot after mutating `globalState` and clearing trial evidence. Publication reuses the active highest-tier set from the already-published authoritative snapshot; usage and candidates never infer or replace admission. The resulting account view exposes `TemporaryUnavailable`, so the next encoded ABI pick excludes the account, delegates fallback, and does not start another trial. A production-path regression test also verifies the authoritative tier/admission is unchanged, while successful request feedback remains a distinct `EvidenceRequestSuccess` path and remains selectable.
