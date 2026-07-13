# S7 final-fix Task 4 report

Base: `447e23b783786f1c876b4506ece58d09d193a6d9`

## Outcome

- Management status now consumes an immutable `ActiveRoster` snapshot instead of reconstructing roster truth from cached accounts.
- Cached cards are intersected with active roster IDs. Removed and lower-tier cache entries are absent; an active-tier cached card remains visible while normal refresh is Dormant.
- The authenticated payload explicitly exposes capability, health, confirmed/provisional state, Degraded, FailClosed, WaitingRoster, background authorization, highest priority, generation, CredentialAmbiguous, and provisional-risk enabled/available/warning state.
- The browser renders lifecycle/risk warnings and round-trips `probe_on_provisional_roster` through both `fillSettings` and `collectSettingsPayload`, so unrelated browser saves retain the current risk option.
- Resource `/status` remains a static shell and skips roster wakeup; no authoritative roster or cached business value is projected through Resource routes.
- Settings and annotations retain their existing write-through-before-success paths.
- Production Management dispatch captures global pointers under `refresherMu`, releases it before `host.auth.list`, uses the wake result directly, and derives credential ambiguity only for durable bindings in the active roster without a host call.

## TDD evidence

### RED: payload/UI contract

Command:

```powershell
go test ./... -run 'TestManagement(UsesActiveRosterOnly|ExposesRosterLifecycle|RoundTripsProvisionalRiskOption)' -count=1
```

Result: exit 1. The test build failed for the expected missing lifecycle-aware APIs: `ManagementLifecycleSnapshot`, `RosterLifecyclePayload`, `HandleManagementRequestWithLifecycle`, and `buildCurrentStatusPayloadWithLifecycle` were undefined.

### RED: production dispatch

Command:

```powershell
go test ./... -run 'TestManagementDispatchUsesImmutableRosterSnapshotWithoutGlobalHostLock' -count=1
```

Result: exit 1. The response contained both `active` and `removed-cache`, and its roster lifecycle was empty. The same test independently proved the pre-existing dispatch did not hold `refresherMu` through the blocked host callback.

### GREEN: focused Task 4

Command:

```powershell
go test ./... -run 'TestManagement(UsesActiveRosterOnly|ExposesRosterLifecycle|RoundTripsProvisionalRiskOption|DispatchUsesImmutableRosterSnapshotWithoutGlobalHostLock)' -count=1
```

Result: exit 0; all packages passed.

### Review follow-up RED: active-scoped CredentialAmbiguous

Command:

```powershell
go test ./... -run TestManagementCredentialAmbiguityReadsDurableChains -count=1
```

Result: exit 1 at compile time because the old global helper accepted no active roster or observation time. The replacement API is active-scoped and the GREEN regression proves that removed ambiguity is ignored, active reconciliation ambiguity is exposed, and an expired reachable credential generation is exposed.

The follow-up reviewer identified one more unresolved-tail edge. A new assertion for `OutcomeUnknown + observed == Next` failed because the generic classifier called it ambiguous even though reconciliation deterministically applies it. The implementation now treats unresolved tails exclusively: Prev and Next are resolvable; only neither is ambiguous; terminal chains continue through the expiry/capacity classifier.

### GREEN: Management security/write-through focus

Command:

```powershell
go test ./... -run 'Test(Management|Status|Settings|Resource|Annotations|SuiteBoundary|MockGroupDBoundary)' -count=1
```

Result: exit 0; all packages passed.

## Verification evidence

### Full suite

```powershell
go test ./... -count=1
```

Exit 0. Fresh post-review final run: main package passed in 9.101s; refactor gate and testsupport passed.

### Race detector

```powershell
go test -race ./...
```

Exit 0. Fresh post-review final run: main package passed in 12.342s; all tested packages passed.

### Vet

```powershell
go vet ./...
```

Exit 0 with no diagnostics.

### S7 gate

```powershell
./scripts/check_refactor_gates.ps1 -Stage S7
```

Exit 0: `S7 roster lifecycle, traceability, K-point, pick-I/O, sensitive-state, and Mock A-E gates passed`.

### Diff checks

```powershell
git diff --check
```

Exit 0. Task source/test scope is limited to `management.go`, `dispatch.go`, `management_test.go`, and `roster_controller_test.go`; this report is the only Task 4 evidence artifact.

## Independent review

The first commit-gate review found one Important issue and no Critical issues: the initial ambiguity heuristic scanned removed chains and its warning overstated §2.5. The fix scopes classification through active roster IDs and durable bindings, handles unresolved reconciliation plus expired-chain ambiguity, and states that conversion freezes while existing non-AuthBlocked credentials remain usable. Follow-up review found the unresolved-Next fallthrough described above; its RED regression and fix are included. Final re-review: **approved, no remaining findings**.

## Invariant notes

- INV-01: Resource status remains shell-only and does not wake or project the roster.
- INV-20 / INV-34: removed and lower-tier cached cards are excluded by the active roster ID intersection.
- INV-23: credential ambiguity is explicit in authenticated Management state and warnings.
- INV-35: WaitingRoster and FailClosed are explicit, including background authorization state.
- INV-37: existing settings and annotation persistence paths remain write-through before success; the risk option uses the same settings path.
