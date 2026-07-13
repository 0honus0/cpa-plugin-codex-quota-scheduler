# S7 Final-Fix Task 5 Report

## Outcome

- Replaced the central self-attesting INV-01..46 tag block with an AST scanner and semantic manifest. Inline `//inv:` comments are ignored everywhere; only explicit owners that resolve to executable top-level `Test...` functions are accepted.
- Missing invariant directions are sorted before failure. Aggregation-only file comments are ignored.
- Replaced `uncovered: []` with the 28 frozen §12 scenario rows: A01-A10, B01-B04, C01-C06, D01-D02, and E01-E06.
- Each §12 row carries one or more existing behavioral owner tests. Validation rejects missing rows, unknown rows, duplicates, missing descriptions, empty owners, non-`Test` owners, and nonexistent owners.
- The S7 PowerShell gate validates traceability/coverage, K-point registries, pick-path I/O, and sensitive-state checks, then derives and runs the exact §12 owner set (48 distinct tests) from JSON.
- Dedicated behavioral owners were added where existing coverage did not execute the frozen scenario.

## Frozen §12 row interpretation

The frozen design is structurally explicit: Group A has ten numbered scenarios; Groups B-E use bold scenario paragraphs as rows. Stable IDs therefore map directly to those source rows:

- A01-A10: the ten numbered §12.A scenarios.
- B01-B04: single-instance matrix, multi-instance matrix, concurrent pick, property-based fallback.
- C01-C06: state transitions, classifier grid, dual-window isolation, crash recovery, time jumps, suppression combinations.
- D01-D02: credential/identity security matrix and Resource boundary scan.
- E01-E06: intent interleavings, refresh timeline, circuit isolation, Degraded boundary, lease matrix, S3 migration drain.

## Explicit invariant mapping rationale

Inline directional tags are historical comments only and are never authoritative evidence. The explicit semantic manifest is the sole invariant owner source, and owners are selected by the behavior they execute rather than filename aggregation:

- Roster/provisional/fencing: INV-02, INV-03, INV-20, INV-21, INV-28, INV-30, INV-33 use provisional request/denial, authoritative-generation, removal fencing, binding re-add, state recovery, and external-login recovery tests.
- Coordinator/Probe: INV-06 through INV-08, INV-14, INV-15, INV-17 through INV-19, INV-27, INV-32, INV-36 use non-coalescing, lease, failure isolation, business-state isolation, dual-window, time-jump, K-point, and suppression tests.
- Refresh/scheduling: INV-10 through INV-13, INV-22, INV-29, INV-41, INV-43 through INV-45 use the independent timeline/oracle, reset-passed classifications, trial CAS/budget, immutable snapshot, and candidates-without-roster-side-effects tests.
- Persistence/Management: INV-16, INV-37 through INV-39 use usage-limit circuit isolation, write-through/failure rollback, and durable fence-ceiling tests.

Positive and negative directions require distinct executable owners. Shared directional owners are rejected by the manifest validator.

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
- `./scripts/check_refactor_gates.ps1 -Stage S7` — pass; 48 distinct exact §12 owner tests executed with roster, traceability, K-point, pick-I/O, and sensitive-state gates.
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
- AST discovery resolves the file's actual `testing` import (including aliases), requires Go's exported Test naming rule, exactly one imported `*testing.T` parameter with any valid identifier, and no result values.
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
- `reread`: fresh reconciliation through the existing outcome-unknown protocol; a classifiable owned rotation advances the cursor and increments TokenEpoch under the normal observation rules.

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

That verdict was superseded by the third re-review described below.

## Third re-review follow-up (supersedes earlier final claims)

The third re-review rejected D01's classifier-only matrix, four invariant mappings, unrestricted credential-resolution exits, Probe due/recovery persistence races, and the literal `t` AST parameter requirement. This wave addressed each finding with RED-first behavioral coverage:

- D01 now drives `CredentialManager.Reconcile` against durable production state for every length-1..4 observation permutation. Owned observations consume the reachable prefix, advance the cursor, update the binding fingerprint, and increment TokenEpoch. External observations reset the chain and increment LoginEpoch/IAE/G while clearing AuthBlocked. Ambiguity auto-clear uses a real reconciliation read; no test mutates `TransitionPhase` directly.
- The four §2.7 branches now prove real published-snapshot pick participation, AuthInstanceID-scoped unresolved annotations with no same-filename inheritance, account-ID-gated quota/Probe eligibility, and access-token-only real-request eligibility.
- All three `/credentials/resolve` actions reject a cursor-only/non-ambiguous active chain before GetAuth or epoch mutation. The core state remains byte-for-byte unchanged and the Management route returns conflict.
- Probe runners are serialized by a shared mutex: Due uses `TryLock` so duplicate wakes remain nonblocking/single-flight, while Recovery holds the same lock across snapshot, recovery, verification, and persistence. The deterministic regression reproduced a concurrent prepared claim under the stale recovery snapshot before the fix.
- INV-01 negative now mutates a real Resource response with sensitive business data and proves the leak guard detects it. INV-10 negative proves `stale_after` alone emits zero requests. INV-11 positive/negative prove Aging emits no request and cannot own a deadline. INV-26 negative executes Probe recovery with `read_start_seq <= send_fence_seq` and proves rejection before publishing a read sequence.
- Executable-test discovery validates the imported `*testing.T` type without requiring the parameter identifier to be `t`.

RED evidence from this wave:

```text
Credential resolution: all three non-ambiguous actions returned nil; route returned 200.
D01: interleaved external observation left persisted chain and epochs unchanged.
§2.7: access-token-only credentials failed with missing chatgpt_account_id.
Probe concurrency: Due claimed probe-...-1 while Recovery held a stale snapshot.
```

The verification below is historical and does not certify the third wave. Fresh final verification and independent review must be appended before commit.

### Fresh third-wave verification

- Focused D01/§2.7/resolution/Probe/invariant/AST owners: pass; root package 5.523s.
- Root package compatibility run: pass; 13.467s.
- Traceability/meta owners: pass; root package 2.019s.
- S7: pass; 48 exact §12 owners; 23.7s wall time.
- Full `go test ./... -count=1 -timeout 180s`: pass; root package 14.334s.
- `go test -race ./... -count=1 -timeout 240s`: pass; root package 27.796s.
- `go vet ./...`: pass.
- `git diff --check`: pass (line-ending warnings only).

The first third-wave review found no Critical issues and three Important gaps. Follow-up RED tests proved that ambiguity could be cleared while a Management read was blocked, grouped `a, b *testing.T` parameters were accepted, and §2.7 did not execute production background paths. The fixes now:

- Revalidate unresolved scope inside the confirm transaction and use `ReconcileUnresolved`, which rechecks the durable chain inside its conditional mutation. Deterministic races for all three actions prove zero mutation after concurrent auto-clear.
- Execute real `OperationQuotaRead` and `OperationProbePrecheck` paths: account-ID credentials issue two HTTP requests; access-token-only credentials issue zero, return `ErrAccountIdentityUnresolved`, and retain trial/pick eligibility.
- Reject a sole AST parameter field containing more than one identifier while accepting any zero/one identifier for imported `*testing.T`.

Fresh post-fix verification:

- Focused reviewer fixes: pass; root package 4.850s.
- Full `go test ./... -count=1 -timeout 180s`: pass; root package 14.866s.
- S7: pass; 48 exact §12 owners; 22.5s wall time.
- `go test -race ./... -count=1 -timeout 240s`: pass; root package 26.855s.
- `go vet ./...`: pass.
- `git diff --check`: pass (line-ending warnings only).

The focused re-review then found one remaining Important case: `ReconcileUnresolved` returned success when its durable chain had been removed before the fresh snapshot. A RED regression reproduced `err=nil`; required reconciliation now returns scope for missing or invalid chains before any success exit.

Fresh final verification after that fix:

- Focused removal/all-action scope tests: pass; root package 2.035s.
- Full `go test ./... -count=1 -timeout 180s`: pass; root package 14.697s.
- S7: pass; 48 exact §12 owners; 22.9s wall time.
- `go test -race ./... -count=1 -timeout 240s`: pass; root package 26.493s.
- `go vet ./...`: pass.
- `git diff --check`: pass (line-ending warnings only).

The next main review found one Critical automatic external-login fencing window, one Important INV-31 negative-owner gap, and the stale inline-tag authority sentence corrected above.

RED evidence:

```text
Automatic external sync: an old in-flight result returned Applied, passed
ValidateWriteback against stale mirrors, and recorded stale.apply while the
external Login/IAE/G commit was durable but not yet published/cancelled.

Forced mirror reload failure: durable Login/IAE/G and the external cursor
remained advanced instead of rolling back.
```

Fixes:

- `ReconcileWithHooks` suspends an external instance with a target-specific cancellation nonce before the durable mutation. Owned rotations do not suspend or advance G/IAE.
- The after-commit hook publishes the committed binding mirror while the credential transaction is serialized. On reload/publish failure, CredentialManager conditionally restores only the exact prior credential chains, bindings, admission epochs, and tier generation; the caller restores only its matching generation+nonce.
- A deterministic later `CancelInstances` race proves rollback cannot undo a newer authoritative cancellation.
- INV-31 negative now maps to `TestConcurrentSameMomentRosterWakesSingleflight`, a distinct top-level eight-wake counterexample that permits exactly one host roster call.

Focused automatic fencing/rollback/cancel and INV-31 tests pass in 2.064s; root package compatibility passes in 14.050s.

Fresh final verification:

- Full `go test ./... -count=1 -timeout 180s`: pass; root package 14.613s.
- S7: pass; 48 exact §12 owners; 23.5s wall time.
- `go test -race ./... -count=1 -timeout 240s`: pass; root package 27.483s.
- `go vet ./...`: pass.
- `git diff --check`: pass (line-ending warnings only).

The independent follow-up found one remaining Important persistence mismatch: after external reconciliation, `PublishAuthoritativeRoster` still built LastConfirmedRoster and activation from the pre-reconcile binding snapshot. RED proved the durable binding at G=6/external fingerprint was paired with a confirmed roster at G=5/old fingerprint. Publication now reloads the durable bindings and tier generation immediately after credential reconciliation, then uses that state for LastConfirmedRoster, activation, Probe recovery, and the runtime roster. The focused restart-facing regression passes in 2.204s.

Fresh final verification after the publish-snapshot fix:

- Full `go test ./... -count=1 -timeout 180s`: pass; root package 15.235s.
- S7: pass; 48 exact §12 owners; 23.6s wall time.
- `go test -race ./... -count=1 -timeout 240s`: pass; root package 27.057s.
- `go vet ./...`: pass.
- `git diff --check`: pass (line-ending warnings only).

Final independent re-review found no Critical, Important, or Minor issues and returned **Ready to merge: Yes**.

## Historical post-approval verification

- Targeted new/meta owners — pass; root 4.223s.
- S7 — pass; 48 exact §12 owners.
- Full `go test ./... -count=1` — pass; root 11.312s.
- `go test -race ./...` — pass; root 22.632s.
- `go vet ./...` — pass.
- `git diff --check` — pass.
