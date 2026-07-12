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

- Benchmark bytes/op increased by 384 while allocations/op stayed constant.
- Legacy dispatch scenarios remain inside nonconstant historical-fixture branches after their S5 replacement assertions; they are not skipped and `go vet` is clean, but can be deleted in a later test-only cleanup.
