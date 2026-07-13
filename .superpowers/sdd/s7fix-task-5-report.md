# S7 Final-Fix Task 5 Report

## Outcome

- Replaced the central self-attesting INV-01..46 tag block with an AST scanner that accepts inline evidence only from executable top-level `Test...` bodies (including tags located in their `t.Run` bodies) or from an explicit owner mapping whose names must resolve to executable `Test...` functions.
- Missing invariant directions are sorted before failure. Aggregation-only file comments are ignored.
- Replaced `uncovered: []` with the 28 frozen §12 scenario rows: A01-A10, B01-B04, C01-C06, D01-D02, and E01-E06.
- Each §12 row carries one or more existing behavioral owner tests. Validation rejects missing rows, unknown rows, duplicates, missing descriptions, empty owners, non-`Test` owners, and nonexistent owners.
- The S7 PowerShell gate validates traceability/coverage, K-point registries, pick-path I/O, and sensitive-state checks, then derives and runs the exact §12 owner set (54 distinct tests) from JSON.
- No assertion-free wrapper or no-op behavior test was added. Existing behavioral coverage was sufficient.

## Frozen §12 row interpretation

The frozen design is structurally explicit: Group A has ten numbered scenarios; Groups B-E use bold scenario paragraphs as rows. Stable IDs therefore map directly to those source rows:

- A01-A10: the ten numbered §12.A scenarios.
- B01-B04: single-instance matrix, multi-instance matrix, concurrent pick, property-based fallback.
- C01-C06: state transitions, classifier grid, dual-window isolation, crash recovery, time jumps, suppression combinations.
- D01-D02: credential/identity security matrix and Resource boundary scan.
- E01-E06: intent interleavings, refresh timeline, circuit isolation, Degraded boundary, lease matrix, S3 migration drain.

## Explicit invariant mapping rationale

Inline directional tags already inside real behavioral suites remain authoritative. The explicit mapping is used where historical tests carried no directional tag or used the old nondirectional `//inv:INV-xx[,INV-yy]` form. Owners were selected by the behavior they execute, not by filename aggregation:

- Roster/provisional/fencing: INV-02, INV-03, INV-20, INV-21, INV-28, INV-30, INV-33 use provisional request/denial, authoritative-generation, removal fencing, binding re-add, state recovery, and external-login recovery tests.
- Coordinator/Probe: INV-06 through INV-08, INV-14, INV-15, INV-17 through INV-19, INV-27, INV-32, INV-36 use non-coalescing, lease, failure isolation, business-state isolation, dual-window, time-jump, K-point, and suppression tests.
- Refresh/scheduling: INV-10 through INV-13, INV-22, INV-29, INV-41, INV-43 through INV-45 use the independent timeline/oracle, reset-passed classifications, trial CAS/budget, immutable snapshot, and candidates-without-roster-side-effects tests.
- Persistence/Management: INV-16, INV-37 through INV-39 use usage-limit circuit isolation, write-through/failure rollback, and durable fence-ceiling tests.

Some positive and negative directions deliberately share one owner when a single behavioral test constructs the protected counterexample and proves both the allowed state and rejected mutation. Examples: Probe sends do not coalesce (INV-06), Probe executes without changing business circuit/trial state (INV-14/15), usage-limit exhaustion updates availability without incrementing circuit failure state (INV-16), and Dormant retains cards while emitting zero normal requests (INV-22).

## TDD evidence

RED was run before scanner/validator implementation:

```text
go test ./... -run 'Test(InvariantTraceabilityRejectsAggregationOnlyTags|MockCoverageRejectsMissingOwner)' -count=1
FAIL: undefined scanInvariantEvidence, mockCoverageMatrix, mockCoverageRow, validateMockCoverage
```

GREEN meta-gates:

```text
go test ./... -run 'Test(InvariantTraceabilityRejectsAggregationOnlyTags|MockCoverageRejectsMissingOwner)' -count=1
PASS
```

The first truthful traceability run also failed with a sorted 64-direction missing report after the central placeholder tags were removed. Adding validated behavioral owners reduced that report to zero.

A second RED fixture used `func TestWrongSignature()` as an owner. It initially passed validation, proving that a `Test` prefix alone was insufficient. The AST discovery now requires the executable Go test shape `func TestX(*testing.T)` with no result values; the fixture then passed by rejecting that owner.

## Verification evidence

- `go test ./... -run TestSuiteRosterManagement -count=1` — pass.
- `go test ./... -run TestInvariantTraceability -count=1` — pass.
- `go test ./... -run '^TestMockCoverage$' -count=1` — pass.
- `./scripts/check_refactor_gates.ps1 -Stage S7` — pass; 54 distinct exact §12 owner tests executed with roster, traceability, K-point, pick-I/O, and sensitive-state gates.
- `go test ./... -run 'TestMockGroup(A|B|C|D|E)' -count=1` — pass.
- `go test -race ./...` — pass; root package 14.313s.
- `go vet ./...` — pass.
- `go test ./... -count=1` — pass; root package 7.520s, below the five-minute virtual-time target.

## Files changed

- `traceability_test.go`
- `testdata/mock_group_coverage.json`
- `scripts/check_refactor_gates.ps1`
- `.superpowers/sdd/s7fix-task-5-report.md`

## Rejected-review follow-up

The first Task 5 review rejected the central directional-tag model and several §12 rows whose owners did not execute the frozen scenario. The follow-up removes those claims rather than preserving aliases.

### Traceability hardening

- Inline `//inv:` comments are no longer accepted as evidence, even inside a `Test...` body. This prevents suite-level bulk tag dumps.
- All legacy invariant comments were removed from behavioral test files. INV-01 through INV-46 now use a semantic manifest with distinct positive and negative executable owners.
- AST discovery resolves the file's actual `testing` import (including aliases), requires Go's exported Test naming rule, exactly one parameter named `t` of imported `*testing.T`, and no result values.
- Meta RED fixtures proved rejection of centralized body tags, missing/fake testing imports, lowercase Test names, wrong signatures, and a shared positive/negative owner.

### Genuine §12 owners added

- A07 `TestSection12A07LegacyManualQuotaConcurrent`: blocks a real legacy lease, submits concurrent manual and interval quota reads, proves exclusion from the envelope, actual-start sequencing, and one shared same-class execution.
- B03 `TestSection12B03ConcurrentPicksOneTrial`: drives 2, 3, and 4 concurrent published-snapshot picks and proves one selected account, one trial, and one evidence refresh intent.
- B04 `TestSection12B04SeededPropertyAndShrink`: compares 512 deterministic seed vectors with the independent oracle and proves a stable two-account minimized regression from a deliberately wrong tie-order mutant.
- C04 `TestSection12C04AllProbePathsAtEveryKPoint`: drives WaitingReset, RetryWait, and AnomalyHold through real deadline/precheck/send transitions for both five-hour and long windows, crosses all six reaching paths with all five Probe K-points, restarts the WAL, and proves zero pre-WAL recovery or one verify-first convergent recovery with the original window/fence.
- D01 `TestSection12D01CredentialIdentityMatrix`: covers chain lengths 1-4 and reachable observation permutations, external interleavings, 4 chain lengths × 3 SaveAuth outcomes × 6 credential K-points, ambiguity auto-clear, all three §2.5 Management exits with epoch assertions, and four identity-resolution branches.
- E01 `TestSection12E01RestrictedEventInterleavings`: exhausts 150 ordered 2-3 event schedules over legacy/manual/interval/precheck/send/verify and asserts instance-lock exclusion, same-class dedupe, actual-start read barriers, and stale writeback fencing.
- INV-12 now has distinct owners for the first all-Unknown/Stale real request and concurrent duplicate-refresh prevention.

### §2.5 Management exits

The review exposed missing frozen behavior, so the follow-up implements the authenticated `/credentials/resolve` Management route:

- `confirm_owned_rotation`: fresh observation, chain cursor reset to the observed fingerprint, TokenEpoch++, Ambiguous clear; LoginEpoch/IAE/G/AuthBlocked unchanged.
- `confirm_external_login`: fresh observation, LoginEpoch++/IAE++/G++, AuthBlocked clear, chain reset; prior admission is cancelled/fenced before the new generation is reactivated.
- `reread`: fresh reconciliation through the existing outcome-unknown protocol with no forced epoch change.

All actions are active-roster scoped, validate the current instance/binding and fingerprint, commit durable state before success, run outside the global refresher lock, reject removed/lower-tier IDs, and append an audit log only after success.

### Additional RED found by behavioral coverage

A07/E01 initially failed because two same-class typed reads queued behind a legacy lease both executed: their actual-start sequence was still zero, so the second request could not join the first. `typedReadEntry` now records its operation class and permits pre-start joining only for the same class, retaining cross-class actual-start barrier behavior.

### Follow-up verification

- Targeted Management/meta/A07/B03/B04/C04/D01/E01/INV-12 owners — pass.
- `./scripts/check_refactor_gates.ps1 -Stage S7` — pass; 48 distinct exact §12 owners.
- `go test ./... -count=1` — pass; root package 9.696s.
- `go test -race ./...` — pass; root package 16.133s.
- `go vet ./...` — pass.
- `git diff --check` — pass.

### Independent review corrections

Independent re-review iterated until no Critical/Important findings remained. Its behavioral findings produced additional RED regressions and fixes:

- A queued same-class typed read with `StartedAfter=100` initially joined a not-yet-started read that later received sequence 100. Pre-start joining is now allowed only for the same class with a zero barrier; nonzero barriers wait for an actual sequence greater than the barrier.
- External confirmation initially held `bindings.mu` while waiting for coordinator cancellation. Cancellation now happens before locks and before the fresh GetAuth observation, preventing both result-loop deadlock and stale credential baselining.
- Management suspension rollback now uses a target-specific cancellation nonce. It restores only its own exact generation+nonce, cannot undo a later authoritative cancellation, and clears typed-read join entries before returning.
- Prepared Probe attempts that crash before the sending WAL write now recover through production `recoverPreparedProbeAttempts` into durable RetryWait. Other C04 paths reload a new controller from durable ProbeWindows, execute verify-first, persist Confirmed windows, and delete the attempt.
- D01 now verifies WAL ordering, persisted planned phase, point/outcome-specific restart reconciliation, and durable Applied/Aborted/Ambiguous terminal behavior rather than merely validating hashes.
- E01 now uses the real BindingRegistry/ValidateWriteback gate, real stale admission/generation rejection, and asserts `read_start_seq > started_after` for every read event.

Final independent verdict: **approved; no remaining Critical or Important findings**.

Fresh post-approval verification:

- Targeted new/meta owners — pass; root 4.223s.
- S7 — pass; 48 exact §12 owners.
- Full `go test ./... -count=1` — pass; root 11.312s.
- `go test -race ./...` — pass; root 22.632s.
- `go vet ./...` — pass.
- `git diff --check` — pass.
