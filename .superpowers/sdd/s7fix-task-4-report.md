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

The follow-up reviewer initially treated the durable binding fingerprint as a fresh observation and concluded that unresolved Prev/Next could be resolved in Management. That conclusion is superseded: the durable binding is stale evidence, so Management must keep every active unresolved tail ambiguous. Fresh Prev/Next/third classification belongs to authoritative synchronization through `CredentialManager.Reconcile`, as documented in the later production-reconciliation follow-up below.

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

## Post-commit main-review follow-up

Main review of `7771991` found three blockers. The selected Management correction is conservative and side-effect free: authenticated Management never treats a durable binding fingerprint as a fresh credential observation and never calls GetAuth or mutates credential WAL state. An active `Planned`/`OutcomeUnknown` tail remains `CredentialAmbiguous` until an authoritative synchronization resolves and persists it. At `d96f3c9`, that production reconciliation call was still missing; the next follow-up adds it.

### Follow-up RED

```powershell
go test ./... -run 'TestManagement(CredentialAmbiguityReadsDurableChains|CredentialAmbiguitySeparatesEmptyRosterAndStoreFailure|DispatchWithoutControllerIsWaitingRoster)' -count=1
```

Exit 1 with all expected failures:

- unresolved active tails with stale durable Prev and Next fingerprints were falsely cleared;
- an empty active roster read a failing store and reported account ambiguity;
- nil-controller authenticated Management returned a legacy cached card with no lifecycle state.

The same test matrix covers stale durable Prev/Next/third fingerprints. All remain ambiguous while unresolved; none is accepted as a fresh reconciliation observation. A general runtime-store error returns no account ambiguity, and an empty roster short-circuits before persistence access.

### Follow-up GREEN and focused integration

```powershell
go test ./... -run 'TestManagement(CredentialAmbiguityReadsDurableChains|CredentialAmbiguitySeparatesEmptyRosterAndStoreFailure|DispatchWithoutControllerIsWaitingRoster)' -count=1
```

Exit 0.

```powershell
go test ./... -run 'Test(Management|Status|Settings|Resource|Annotations|SuiteBoundary|MockGroupDBoundary|SuiteRuntimeWiring|ProductionProvisional|Binding)' -count=1
```

Exit 0; main package passed in 3.173s and all selected packages passed.

### Follow-up final verification

- `go test ./... -count=1`: exit 0; main package 7.316s.
- `go test -race ./...`: exit 0; main package 11.551s.
- `go vet ./...`: exit 0, no diagnostics.
- `./scripts/check_refactor_gates.ps1 -Stage S7`: exit 0; S7 roster lifecycle, traceability, K-point, pick-I/O, sensitive-state, and Mock A-E gates passed.
- `git diff --check`: exit 0.

Independent follow-up review: **approved, no blockers**. It confirmed conservative unresolved-tail behavior, empty-roster short-circuit, non-ambiguous store failure, explicit nil-controller Capability-B/WaitingRoster projection with zero cards, and unchanged Resource shell behavior.

## Production CredentialAmbiguous auto-resolution follow-up

Approval review then identified the missing frozen §2.5 runtime mechanism: production constructed `CredentialManager` but no authoritative synchronization called `Reconcile`, so unresolved tails could remain frozen forever and later credential saves returned `ErrCredentialUnresolved`.

### RED: restart and authoritative sync

```powershell
go test ./... -run 'TestProductionRosterSync(ReconcilesCredentialTailsAfterRestart|CredentialReconcileFailureIsPerInstance)' -count=1
```

Exit 1 with the expected evidence: every case made zero credential GetAuth calls, Prev/Next remained `OutcomeUnknown`, and a healthy instance remained `Planned` when another active instance had a host error.

Production now calls the existing `CredentialManager.Reconcile` for each sorted active binding immediately after authoritative highest-tier binding reconciliation and before roster publication. Reconciliation uses fresh GetAuth and the existing WAL protocol:

- observed Prev persists `Aborted`;
- observed Next persists `Applied`;
- a third fingerprint remains unresolved and Management-visible as ambiguous;
- a host error remains unresolved, is logged generically, and does not block unrelated active instances or roster publication;
- terminal cases have a usable `SaveTail`; ambiguous/error cases retain `ErrCredentialUnresolved`;
- Management projection performs no credential host call or persistence mutation.

### Commit-gate stale-state RED

The first review of that wiring found an AuthIndex-reset interleaving: `BindingRegistry` could replace the durable chain while `CredentialManager` retained the old cached unresolved transition.

```powershell
go test ./... -run TestProductionRosterSyncAuthIndexChangeDoesNotReconcileResetChain -count=1
```

Exit 1: publication made two GetAuth calls instead of the single binding-replacement observation, proving stale manager reconciliation ran after the chain reset.

`CredentialManager.Reconcile` now reloads `PersistentSnapshot` under its lock before inspecting a chain. Terminal persistence searches and revalidates `SaveSeq`, Prev, Next, and unresolved phase rather than indexing a cached position. A concurrently replaced/reset transition returns a neutral report without resurrecting old WAL state. The AuthIndex-change regression is GREEN and proves the reset chain remains empty at the new cursor with exactly one GetAuth.

### Final evidence

- Focused credential/WAL/roster/Management: `go test ./... -run 'Test(Credential|ProductionRosterSync|Management|SuiteRosterManagement|SuiteRuntimeWiring|Binding)' -count=1` — exit 0, main package 2.892s.
- Full: `go test ./... -count=1` — exit 0, main package 7.868s.
- Race: `go test -race ./...` — exit 0, main package 13.079s.
- Vet: `go vet ./...` — exit 0, no diagnostics.
- S7: `./scripts/check_refactor_gates.ps1 -Stage S7` — exit 0; all roster lifecycle, traceability, K-point, pick-I/O, sensitive-state, and Mock A-E gates passed.
- Diff: `git diff --check` — exit 0.
- Independent final re-review: **approved, no blockers**.

## Invariant notes

- INV-01: Resource status remains shell-only and does not wake or project the roster.
- INV-20 / INV-34: removed and lower-tier cached cards are excluded by the active roster ID intersection.
- INV-23: credential ambiguity is explicit in authenticated Management state and warnings.
- INV-35: WaitingRoster and FailClosed are explicit, including background authorization state.
- INV-37: existing settings and annotation persistence paths remain write-through before success; the risk option uses the same settings path.
