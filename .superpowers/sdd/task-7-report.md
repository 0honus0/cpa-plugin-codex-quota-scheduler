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

## Corrected production cutover review

The initial S6 commit exposed only primitives and was not an active cutover. The correction extends the S3 coordinator additively under DEV-008 and wires the complete production path:

- `NewProductionQuotaRefresher` loads persistent Probe windows/attempts, constructs `ProbeController`, `ProbeWAL`, and the global `FenceAllocator`, and schedules Probe deadlines independently of normal-refresh mode.
- Typed coordinator classes are `quota_read`, `probe_precheck`, `probe_send`, and `probe_verify`. Actual `ReadStartSeq` is allocated only when a new read starts after instance-lock waiting. It remains distinct from legacy `completedFence`.
- Barrier reads join only when `ReadStartSeq > StartedAfter`; verify reads claim an attempt and never join across attempts. Probe sends always receive a unique nonjoin key.
- Production sequence is GetAuth/read precheck, classification, persistent send fence, fsynced `sending`, Probe HTTP, fsynced `sent`, injectable propagation wait retaining the instance lease while releasing the global slot, then barrier verify and persisted window mutation.
- Startup recovery consumes persisted sending/sent/SentUnknown attempts through the typed coordinator. Suppression applies to normal and recovery paths; lease reclaim persists SentUnknown and never re-signs the send.
- Capability-B remains fail-closed/WaitingRoster. AuthBlocked is authoritative in `BindingRegistry`; only a strictly greater externally observed LoginEpoch clears it.
- Due Probe ignores circuit, exhaustion, temporary-unavailable, and trial state, and the production test asserts circuit counters/state and trial state remain unchanged.
- Normal held refresh transactions now carry `scheduler_initial`, `scheduler_interval`, `scheduler_stale_recovery`, or `manual_refresh`. The old wire label remains only for `OperationLegacyRefresh` compatibility until that S3 adapter is deleted; typed Probe cannot submit it.

### Corrected oracle and matrices

- All 864 golden rows now materialize semantic tagged baselines, reset timestamps, known/unknown lengths, usage observations, and delay-relative clocks. Every production classifier result is compared to an independent ordered test oracle before JSON equality is accepted.
- The state suite asserts expected state/existence/intent count for every 10-state × event row and independent expected results for the 10×10 dual-window product.
- Time jumps cover before, exact cutoff, and many-cutoff jumps without replay. Suppression covers normal/recovery/SentUnknown entry semantics and Activated/StillLazy/Ambiguous verification results.

### Corrected crash and coordinator coverage

- Production K-point tests traverse the real orchestrator for all five Probe WAL/HTTP boundaries, reconstruct from the same runtime state, recover verify-first, and assert at most one Probe POST.
- Typed Mock E covers a quota read queued before send, actual-start sequence allocation after the send fence, and legal barrier joining. Mock A scenario 7 covers the legacy envelope lease concurrent with typed reads.
- Propagation tests separately prove lease retained, HTTP slot released, virtual-clock completion, and lease reclaim → SentUnknown/no resign.
- DEV-008 is recorded in `docs/deviations.md`.

### Corrected verification

- S1, S2, S2.5, S3, S4, S5, S6 gates: pass; original S3 tests unchanged.
- Focused production Probe/typed coordinator suite: pass.
- Probe/typed race suite: pass.
- Full `go test ./...`: pass.
- `go vet ./...`: pass.
- `git diff --check`: pass with only CRLF conversion warnings.

## Final release-blocker correction

- Due deadlines are applied and persisted before orchestration; WaitingReset, RetryWait, and AnomalyHold enter PendingCheck exactly once without deadline replay/spin.
- Added the additive `probe_sequence` coordinator job. Precheck, WAL-before-send, POST, sent WAL, propagation wait, causal verify, classification, and persistence now execute under one instance lease. Propagation still releases the global HTTP slot, and queued same-instance writes remain blocked until verify/release.
- Every run intersects bindings with the currently confirmed highest Codex tier. Removed/demoted instances lose windows and attempts without issuing requests.
- Durable `prepared` claims plus coordinator singleflight collapse concurrent startup/timer/wake triggers. `ProbeAttemptSeq` supplies durable monotonic IDs even with a frozen clock.
- Persisted sending/sent/SentUnknown attempts are exclusively recovery-owned and verify-first regardless of suppression expiry. Recovery preserves the original AttemptID.
- Send, status, propagation, verify, and sent-WAL failures retain recoverable SentUnknown state; pre-send failures enter bounded RetryWait. Per-instance failures are accumulated while remaining instances continue.
- Probe 401 uses authoritative BindingRegistry AuthBlocked persistence and stamps window LoginEpoch; only a strictly greater external LoginEpoch resumes.
- PersistentSnapshot failures are surfaced by both due and recovery paths.
- Fixed coordinator reclaim routing so `probe_sequence` SentUnknown inheritance cannot be overwritten by the legacy adapter.

### Final regression evidence

- `TestProbeDeadlineConsumedOnceWithoutSpin`: pass.
- `TestProbeSequenceHoldsInstanceLeaseThroughVerify`: pass.
- `TestProbeAttemptIDsMonotonicWithFrozenClock`: pass.
- Focused production Probe and K-point crash/restart recovery: pass.
- Probe/typed race suite: pass.
- S1, S2, S2.5, S3, S4, S5, S6 gates: pass.
- Full `go test ./...`, `go vet ./...`, and `git diff --check`: pass; diff-check emitted only CRLF conversion warnings.

## Final scheduler/orphan review correction

- Deadline transitions now clear the consumed deadline in the same controller mutation that enters PendingCheck. `NextDeadline` considers only WaitingReset, RetryWait, and AnomalyHold, so PendingCheck and other non-timed states cannot produce an expired zero-delay timer.
- A production propagation test holds the sequence lease at the three-second wait, verifies the scheduler deadline is zero, repeatedly invokes the due path, and proves no additional HTTP work starts before verify/release.
- Due and recovery now reconcile the union of all persisted ProbeWindows and ProbeAttempts keys against the current confirmed highest-tier binding-instance set before per-instance work. Restarted, removed, demoted, and binding-less orphan records are durably deleted without requests.
- Once iteration begins, orphan cleanup, durable claim, recovery snapshot, completion, and final persistence errors preserve the first error while safe work continues for other confirmed instances. Two-instance due and recovery regressions inject a phase failure and prove the peer instance still sends/verifies or completes recovery.
- Removed the superseded production `probe_send` payload/executor branch. Normal production orchestration is exclusively `probe_sequence`; standalone typed read classes remain for recovery/coordinator compatibility.

### Final scheduler/orphan verification

- Focused deadline, propagation, orphan restart/demotion, two-instance continuation, and dead-handler regressions: pass.
- Probe suite, K-point recovery, Mock A/E, and unmodified S3 coordinator suite: pass.
- Probe/typed race suite: pass.
- S1, S2, S2.5, S3, S4, S5, and S6 gates: pass.
- Full `go test ./...`, `go vet ./...`, and `git diff --check`: pass; CRLF conversion warnings only.
