# Task 7 / S6 Report

## Outcome

Replaced refresh-owned Probe execution with an independent dual-window controller, ordered classifier, persistent window/attempt schema, and crash-safe verify-first WAL recovery. The S3 `ProbeAttemptSeam` was evolved as a strict superset; existing `SentUnknown` records retain every field.

## RED / GREEN

- RED 1: focused Probe suite failed to compile on missing baseline, classifier, controller, attempt phases, and WAL APIs.
- GREEN 1: added tagged baselines, P1-P5 dispatch shape, UsageOnly table, ordered Reset rules, ten-state controller, and WAL recovery.
- RED 2: removing refresh-owned Probe caused five legacy-envelope expectations to fail.
- GREEN 2: rewrote the exact legacy envelope assertion and removed four obsolete refresh-owned tests after mapping each to S6 controller/classifier/WAL replacements below.
- RED 3: initial golden file generation differed due ordering/encoding.
- GREEN 3: test-side independent generator now writes and compares stable UTF-8 JSON.

## Golden oracle

- Rows: 864 (3 baseline types × 8 reset offsets × 3 lengths × 4 usage changes × 3 delay positions).
- Generator is test-side and does not call `ClassifyProbeWindow`.
- Stable tuple sort and JSON diff: empty.

## State transitions and time

- Persistent states: Idle, WaitingReset, PendingCheck, SentAwaitingVerify, SentUnknown, RetryWait, Confirmed, AuthBlocked, AnomalyHold, WaitingRoster.
- Machine coverage includes all 10 states × 7 event kinds and the complete 10×10 dual-window state product with one response independently fed to both windows.
- Illegal event regression asserts zero intent and zero mutation.
- Deadline behavior is independent of normal-refresh Dormant mode.
- Recovery covers before-grace, exact-grace, and suppression behavior without sleeps.

## WAL, recovery, and K points

- `sending` is persisted through the fsync state store before HTTP permission.
- Known success supports immediate `sent` persistence.
- Recovery of `sending`, `sent`, and `sent_unknown` emits quota-read intents with `started_after = send_fence_seq`; `sending` converges to SentUnknown.
- Suppression is at least 10 minutes from `created_at`.
- Registered/reachable S6 points: `K_PROBE_SENDING_WRITE`, `K_PROBE_AFTER_SENDING`, `K_PROBE_BEFORE_HTTP`, `K_PROBE_AFTER_HTTP`, `K_PROBE_SENT_WRITE`.
- `TestS6KPointRegistryMatchesSource` fails on source/registry count or name mismatch.

## Mock ownership

- Group C: `TestMockGroupC`, golden classifier, all-state/event matrix, 10×10 dual-window product.
- Group A: `TestMockGroupAProbeRecovery`, K-point injection, verify-first and suppression recovery.

## Legacy replacement mapping

- `TestRefreshDueRunsResetProbeForLazyWindow` → classifier ordered rules + dual-window controller.
- `TestRefreshDueFailedResetProbeBacksOff` → controller verify RetryWait and independent retry state.
- `TestRefreshDueDoesNotProbeActiveWindow` → ActivatedNew/ActivatedInferred classifier/controller cases.
- `TestAdmissionChangeAfterProbePreventsPostProbeQuotaRequest` → legacy envelope now proves no Probe/post-read; causal recovery uses attempt/fence identity.
- `TestLegacyRefreshCompleteOutboundEnvelope` rewritten to require only GetAuth, token refresh, SaveAuth, quota read, and reset-credits.

## Verification

- `go test ./... -run TestSuiteProbe -count=1` — pass.
- `go test -race ./... -run 'TestProbe' -count=1` — pass.
- `./scripts/check_refactor_gates.ps1 -Stage S6` — pass.
- `go test ./...` — pass.
- `git diff --check` — pass (Git emitted only CRLF conversion warnings).

## Files and self-review

- Added: `probe_state.go`, `probe_state_test.go`, `probe_classify.go`, `probe_classify_test.go`, `probe_wal.go`, `probe_recovery_test.go`, `testdata/probe_classify_golden.json`.
- Modified: `refresh.go`, `coordinator.go`, `runtime_state.go`, `state_store.go`, legacy tests, and S6 gate.
- Probe success does not touch scheduler trials or business circuit fields.
- S7 roster TTL/Degraded lifecycle was not implemented.

## Concerns

- The controller produces coordinator intents and persistence primitives, but runtime orchestration of every HTTP phase remains deliberately separated from S7 roster lifecycle.
- `LegacyRefreshSource` remains for S3 compatibility tests, while Probe source is now accepted independently; the legacy refresh envelope itself no longer performs Probe calls.
