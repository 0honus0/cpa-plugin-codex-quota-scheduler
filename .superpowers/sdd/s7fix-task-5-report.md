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
