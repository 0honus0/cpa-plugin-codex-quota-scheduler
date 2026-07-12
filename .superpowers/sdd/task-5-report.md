# Task 5 / S4 Report — Dormant/Active Normal Refresh

## RED / GREEN

- RED 1: `TestSuiteRefresh`, source truth table, and `TestMockGroupERefresh` failed to compile because the controller, modes, cache snapshot, and intent sources did not exist.
- GREEN 1: controller tests passed with the exact virtual timeline and nanosecond boundary checks.
- RED 2: `TestNormalRefreshDeadlineHasSingleControllerOwner` observed legacy `NextRefreshDueAt=10:30`; `TestRefreshActiveWindowDeadlineIsExclusive` observed Active at the exact cutoff.
- GREEN 2: removed legacy interval/stale/never-refreshed deadlines and made the active cutoff exclusive.
- Full-suite RED after the ownership change identified only superseded expectations for interval fallback/startup refresh. Those tests were rewritten to assert controller ownership and Dormant startup, then the full suite passed.

## Timeline / Source Matrix

Timeline (`interval=30m`, `window=1h`):

| Virtual time | Event | Mode/result |
|---|---|---|
| 10:00 | real pick, initial cache | Active; initial intent |
| 10:20 | real pick | Active extended to 11:20; no refresh |
| 10:30 | controller deadline | interval intent |
| 11:00 | controller deadline | interval intent |
| 11:20 | active-window deadline | Dormant; no intent/request |

Unique source priority for all eight boolean combinations is: `initial > stale_recovery > interval > none`. Exactly one source is returned.

## Invariants

- INV-10/11: `stale_after` and Aging are classification/pick-recovery facts only; neither owns a deadline.
- INV-22: Dormant transition does not delete accounts, cached quota, annotations, or Management-visible cards.
- INV-32: Dormant normal refresh exits before host auth listing or OpenAI HTTP.
- S3 safety preserved: coordinator admission still requires `LegacyRefreshSource == legacy_refresh_txn`; controller source labels are selection metadata and are not submitted as alternate coordinator sources.

## Verification

- `go test ./... -run TestSuiteRefresh -count=1` — PASS.
- `./scripts/check_refactor_gates.ps1 -Stage S4` — PASS.
- targeted refresh race suite — PASS.
- `go vet ./...` — PASS.
- `go test ./...` — PASS.
- pick-path AST ratchet: actual 1, baseline 8; no added violation.
- `git diff --check` — PASS.

## Files

- Added: `refresh_controller.go`, `refresh_controller_test.go`.
- Modified: `refresh.go`, `dispatch.go`, `config.go`, `config_test.go`, `state.go`, `state_test.go`, `refresh_test.go`, `scripts/check_refactor_gates.ps1`.

## Self-review / Concerns

- The controller uses only virtual `time.Time` inputs; tests use no sleeps. The existing production loop uses a timer only for an owned deadline and has no Dormant fallback polling timer.
- Reset/retry/circuit/probe auxiliary behavior remains in the legacy state deadline path. This task removed only competing normal interval/stale/initial timing; Probe redesign remains intentionally deferred to S6.
- Existing unrelated untracked `.superpowers` artifacts were not staged.
