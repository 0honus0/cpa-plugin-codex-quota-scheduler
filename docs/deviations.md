# Refactor deviations

| ID | Spec clause | Conflict | Conservative decision | Tests | Status |
| --- | --- | --- | --- | --- | --- |
| DEV-001 | Plan Task 1 RED | The Resource/Management boundary already satisfies INV-01, so forcing a production-code failure would weaken known-good behavior. | Characterization test + mutation validation replaces forced RED. S1 onward remains strict RED -> GREEN. | `TestSuiteBoundary/mutation_scanner_detects_a_test-owned_leak` | Accepted |
| DEV-002 | Plan Task 2 Step 5 | The current transitive pick path still reaches legacy refresh code, so immediate zero-violation enforcement would either misrepresent the path or pull the S5 rewrite into S1. | S1 uses a standard-library Go AST analyzer rooted at real `handleSchedulerPick`, discovers the same-package transitive call closure and import aliases, and checks a counted file/type/symbol ratchet baseline. S5 requires both baseline and actual closure violations to be empty. | `go test ./internal/refactorgate`; `./scripts/check_refactor_gates.ps1 -Stage S1 -VerifyNegativeFixture`; future `-Stage S5` | Accepted |
| DEV-003 | Design section 1.1 | An empty, missing, or null roster has no entry whose Priority presence can prove the authoritative Priority contract. | Treat `{}`, missing/null/empty `files`, malformed payloads, zero normalized entries, or any entry missing explicit Priority as Capability-B regardless of provider. | `TestSuiteCapabilityFallsBackForEmptyRoster`; `TestSuiteCapabilityRawRosterPayloadsCannotProveCapability`; `TestSuiteCapabilityFallsBackWhenNonCodexPriorityIsMissing` | Accepted |
| DEV-004 | Design section 11 external constants | Host callback readiness is undocumented and a hanging callback could otherwise leave startup detection unresolved forever. | Bound the startup probe at the fixed external constant `hostRosterDetectionTimeout = 5s`; timeout deterministically publishes Capability-B. Recovery remains owned by S7. | `TestSuiteCapabilityStartupTimeoutPublishesFallbackWithoutRealSleep`; future `-Stage S7` | Accepted |

## Permanent execution-review rule

Reviewers judge the contract of the current frozen-plan stage. A finding owned
by a later stage becomes a machine-checkable acceptance item for that stage and
does not block the current stage unless the current-stage output itself
violates an invariant. The gate script encodes the currently known S5 and S7
acceptance items; S1 does not execute those future-stage branches.

### Future-stage machine checklist

- S5: `scripts/refactor_gates/s1-pick-path-baseline.json` must be empty; the
  INV-43 pick-path scan must report zero violations; and
  `TestSchedulerPickABIPathSnapshotOnly` must exercise the real ABI
  `scheduler.pick` path under snapshot-only constraints.
- S7: `TestStartupCapabilityBRecoversThroughRosterSynchronization` must prove
  that an initial startup Capability-B later becomes Capability-A through the
  roster-controller synchronization policy.
