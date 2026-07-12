# Codex Quota Scheduler Core Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the frozen v5 scheduler refactor through serial stages S0–S7, with machine-enforced security, identity, persistence, coordination, refresh, scheduling, Probe, roster, and Management invariants.

**Architecture:** Three controllers—roster synchronization, normal quota refresh, and Probe—submit typed intents to one coordinator. The coordinator owns mutable state, authentication, deduplication, leases, global HTTP concurrency, WAL/fence ordering, and fenced writeback; `scheduler.pick` reads an immutable atomic snapshot and performs no I/O. The authoritative design is `docs/superpowers/specs/2026-07-12-quota-scheduler-core-refactor-design.md`.

**Tech Stack:** Go 1.26, CLIProxyAPI plugin SDK v7.2.42, Go `testing`, `sync/atomic`, JSON state files, PowerShell static checks, embedded HTML/JavaScript Management UI.

## Global Constraints

- Execute stages strictly in order: S0 → S1 → S2 → S3 → S4 → S5 → S6 → S7.
- Treat INV-01 through INV-46 as normative; each invariant requires at least one positive and one negative `//inv:INV-xx` test tag.
- Resolve implementation conflicts by invariant table > state machine or decision table > prose > example.
- When unresolved, prefer no outbound request, discard writeback, and wait or back off; record the decision in `docs/deviations.md`.
- Do not modify CPA source code.
- Do not use `scheduler.pick.candidates` to mutate roster membership, tier generation, or admission epochs.
- Keep the pick path free of network I/O, disk I/O, locks that wait on background work, and goroutine joins.
- Resource routes serve static shell assets only; business data and mutations remain behind Management authentication.
- Preserve the frozen constants in design §11 exactly.
- Use virtual time in tests; no test may depend on real sleeps.
- Do not push, tag, release, publish, or mutate remote issues while executing this plan unless separately authorized.
- Treat the upstream CPA contract request as a documented deferred action; local S1 completion does not imply remote submission.

---

## Planned File Structure

- `boundary_test.go`: S0 Resource/Management boundary suite using runtime sentinel values.
- `capability.go`, `capability_test.go`: runtime Capability-A/B detection and host roster normalization.
- `pick_benchmark_test.go`: pick concurrency and allocation baseline artifact.
- `identity.go`, `identity_test.go`: identity/instance/epoch model and credential classification.
- `credential_wal.go`, `credential_wal_test.go`: four-state SaveAuth transition protocol.
- `state_store.go`, `state_store_test.go`: schema migration, atomic write, backup, corruption recovery.
- `fence.go`, `fence_test.go`: monotonic sequence block reservation and fsync-before-issue rule.
- `testsupport/clock.go`: virtual clock with deterministic advance and deadline delivery.
- `testsupport/scheduler.go`: deterministic event loop and controlled interleaving explorer.
- `testsupport/fakehost.go`: scripted CPA roster/auth host with failures and call accounting.
- `testsupport/fakeopenai.go`: scripted usage/reset-credits/Probe transport with response and delay injection.
- `testsupport/kpoints.go`: crash-point registry and injected crash controller shared by persistence, coordinator, and Probe tests.
- `coordinator.go`, `coordinator_test.go`: intent loop, leases, deduplication, HTTP slots, execution fencing.
- `auth_transaction.go`, `auth_transaction_test.go`: complete legacy refresh envelope and drain adapter.
- `refresh_controller.go`, `refresh_controller_test.go`: Dormant/Active normal refresh state machine.
- `selection.go`, `selection_test.go`: Preferred/Opportunistic/Excluded classification and ordering.
- `trial.go`, `trial_test.go`: optimistic trial CAS and evidence budget.
- `scheduler_snapshot.go`, `scheduler_snapshot_test.go`: immutable pick snapshot publication.
- `probe_state.go`, `probe_classify.go`, `probe_wal.go`: dual-window Probe state machine.
- `probe_state_test.go`, `probe_classify_test.go`, `probe_recovery_test.go`: Probe transition, golden, crash, and suppression suites.
- `testdata/probe_classify_golden.json`: independently generated Probe classification oracle rows.
- `roster_controller.go`, `roster_controller_test.go`: TTL, Degraded, FailClosed, tier replacement, and Capability wiring.
- `traceability_test.go`: INV positive/negative tag matrix and K-point discovery gate.
- `testdata/mock_group_coverage.json`: generated ownership matrix for every §12 A–E scenario row.
- `scripts/check_refactor_gates.ps1`: pick-I/O, sensitive-state, traceability, and K-point static checks.
- `docs/deviations.md`: implementation-time conservative decisions; begin with an empty registry.
- Existing integration points remain in `main.go`, `dispatch.go`, `refresh.go`, `scheduler.go`, `state.go`, `disk_state.go`, `management.go`, and their current tests.

---

### Task 1: S0 — Freeze the Resource/Management Boundary

**Files:**
- Create: `boundary_test.go`
- Create: `scripts/check_refactor_gates.ps1`
- Create: `docs/deviations.md`
- Modify: `management.go`
- Modify: `management_test.go`

**Interfaces:**
- Consumes: `HandleManagementRequest`, `isResourcePath`, `resourceRouteAllowed`, and the embedded status page.
- Produces: `TestSuiteBoundary`, §12.D alias `TestMockGroupDBoundary`, and static gate `scripts/check_refactor_gates.ps1 -Stage S0`.

- [ ] **Step 1: Add the failing boundary suite**

Create `boundary_test.go` with unique runtime business-data sentinels. Seed those values into the fake plugin state, request every registered Resource route without a Management key, and assert that no response contains any sentinel. JavaScript field names such as `quota`, `reset_at`, and `scheduler_priority` are allowed because INV-01 prohibits leaked business values, not schema identifiers:

```go
type BoundarySentinels struct {
    AuthID       string
    AccountID    string
    Alias        string
    QuotaValue   string
    ResetRFC3339 string
    LogMessage   string
}

func (s BoundarySentinels) Values() []string {
    return []string{s.AuthID, s.AccountID, s.Alias, s.QuotaValue, s.ResetRFC3339, s.LogMessage}
}

func TestSuiteBoundary(t *testing.T) {
    //inv:INV-01 positive
    sentinels := BoundarySentinels{
        AuthID:       "SENTINEL_AUTH_X9K",
        AccountID:    "SENTINEL_ACCOUNT_Q7M",
        Alias:        "SENTINEL_ALIAS_P4V",
        QuotaValue:   "987654321",
        ResetRFC3339: "2099-12-31T23:58:57Z",
        LogMessage:   "SENTINEL_LOG_N6R",
    }
    restore := seedBoundarySentinelsForTest(t, sentinels)
    t.Cleanup(restore)

    for _, route := range registeredResourceRoutesForTest(t) {
        body := requestResourceForTest(t, route)
        for _, sentinel := range sentinels.Values() {
            if strings.Contains(body, sentinel) {
                t.Fatalf("%s leaked runtime sentinel %q", route, sentinel)
            }
        }
    }

    //inv:INV-01 negative
    resp := requestResourceMutationForTest(t, "/refresh")
    if resp.StatusCode < 400 {
        t.Fatalf("resource mutation status = %d", resp.StatusCode)
    }
}

func TestMockGroupDBoundary(t *testing.T) {
    TestSuiteBoundary(t)
}
```

- [ ] **Step 2: Run RED**

Run:

```powershell
go test ./... -run TestSuiteBoundary -count=1
```

Expected: FAIL because the sentinel-seeding helpers do not exist, or because a Resource response contains a seeded runtime value.

- [ ] **Step 3: Make Resource responses static-shell-only**

Refactor `management.go` so Resource handlers return only the embedded shell document and static asset metadata. Keep every status/settings/log/annotation payload and every mutation under `/v0/management/plugins/codex-quota-scheduler`. The page may contain JavaScript field names used after authenticated loading, but must not embed runtime account values.

- [ ] **Step 4: Add the first machine gate**

Create `scripts/check_refactor_gates.ps1` with parameters `Stage` and `RepoRoot`. For S0, run `go test ./... -run TestSuiteBoundary -count=1` and fail on a nonzero exit code. Use `& go test ...; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }`.

- [ ] **Step 5: Create the deviation registry**

Create `docs/deviations.md` with columns `ID | Spec clause | Conflict | Conservative decision | Tests | Status` and no open entries.

- [ ] **Step 6: Run GREEN and regression tests**

Run:

```powershell
./scripts/check_refactor_gates.ps1 -Stage S0
go test ./...
```

Expected: both commands exit 0.

- [ ] **Step 7: Commit S0**

```powershell
git add boundary_test.go management.go management_test.go scripts/check_refactor_gates.ps1 docs/deviations.md
git commit -m "test: freeze management security boundary"
```

---

### Task 2: S1 — Code Runtime Capability Detection and Pick Baseline

**Files:**
- Create: `capability.go`
- Create: `capability_test.go`
- Create: `pick_benchmark_test.go`
- Create: `docs/baselines/pick-hot-path.md`
- Create: `docs/upstream/host-roster-contract-request.md`
- Modify: `main.go`
- Modify: `dispatch.go`
- Modify: `scripts/check_refactor_gates.ps1`

**Interfaces:**
- Produces:
  - `type HostCapability uint8` with `CapabilityA` and `CapabilityB`.
  - `type HostRosterSnapshot struct { Capability HostCapability; Entries []RosterEntry; ConfirmedAt time.Time }`.
  - `func DetectHostRoster(ctx context.Context, host HostAuthLister, now time.Time) HostRosterSnapshot`.
  - `func HighestCodexTier(entries []RosterEntry) (priority int, ids []string, ok bool)`.

- [ ] **Step 1: Write capability tests**

Cover successful `host.auth.list` with explicit Priority, host failure, missing Priority capability, non-Codex filtering, equal highest priorities, and empty results. Tag INV-31/34 tests where the fake host counts calls and where lower-tier IDs are excluded.

- [ ] **Step 2: Run RED**

```powershell
go test ./... -run TestSuiteCapability -count=1
```

Expected: build failure because capability types and detection functions are absent.

- [ ] **Step 3: Implement detection without startup blocking**

Implement `HostAuthLister` as the narrow adapter around the existing host callback. Return Capability-B on call error or missing Priority presence; never wait for an upstream answer and never inspect pick candidates to determine capability.

- [ ] **Step 4: Add the pick benchmark and concurrency test**

Add `BenchmarkSchedulerPickSnapshot` and `TestSchedulerPickConcurrentSnapshotOnly`. The fake coordinator publishes a snapshot before the benchmark; the benchmark must perform selection only. Record `ns/op`, `B/op`, and `allocs/op` in `docs/baselines/pick-hot-path.md` with the exact command and commit.

- [ ] **Step 5: Extend the static gate**

For S1, scan the transitive pick entry files for forbidden calls: `http.`, `os.Open`, `os.ReadFile`, `os.WriteFile`, `time.Sleep`, `WaitGroup.Wait`, and host roster calls. Allow only the atomic trial CAS helper and bounded nonblocking intent enqueue.

- [ ] **Step 6: Run GREEN**

```powershell
go test ./... -run TestSuiteCapability -count=1
go test -race ./... -run TestSchedulerPickConcurrentSnapshotOnly -count=1
go test -bench BenchmarkSchedulerPickSnapshot -benchmem -run '^$'
./scripts/check_refactor_gates.ps1 -Stage S1
```

Expected: all commands exit 0 and the baseline document contains measured values.

- [ ] **Step 7: Record the deferred upstream request**

Create `docs/upstream/host-roster-contract-request.md` containing the requested stable Priority roster contract, incarnation/revision, tombstone/change sequence, and `SaveAuth(expected_revision)` CAS. Mark its remote submission as `Deferred: requires explicit user authorization`. S1 is not blocked by submission because Capability-A/B are both implemented.

- [ ] **Step 8: Commit S1**

```powershell
git add capability.go capability_test.go pick_benchmark_test.go docs/baselines/pick-hot-path.md docs/upstream/host-roster-contract-request.md main.go dispatch.go scripts/check_refactor_gates.ps1
git commit -m "feat: detect authoritative host roster capability"
```

---

### Task 3: S2 — Introduce Identity, WAL, Persistence, and Fence Primitives

**Files:**
- Create: `identity.go`, `identity_test.go`
- Create: `credential_wal.go`, `credential_wal_test.go`
- Create: `state_store.go`, `state_store_test.go`
- Create: `fence.go`, `fence_test.go`
- Create: `testsupport/clock.go`, `testsupport/scheduler.go`
- Create: `testsupport/fakehost.go`, `testsupport/fakeopenai.go`, `testsupport/kpoints.go`
- Modify: `models.go`, `state.go`, `disk_state.go`
- Modify: `scripts/check_refactor_gates.ps1`

**Interfaces:**
- Produces:
  - `AccountIdentity`, `AuthInstanceID`, `InstanceAdmissionEpoch`, `TierGeneration`, `AuthBindingEpoch`, `LoginEpoch`, `TokenEpoch`, `ExecutionToken`.
  - `CredentialFingerprint{SubjectHash, RefreshTokenHash, MetadataHash, CompositeHash [32]byte}`.
  - `CredentialTransition{Prev, Next CredentialFingerprint; SaveSeq uint64; Phase TransitionPhase}`.
  - `ClassifyObservedCredential(chain TransitionChain, observed CredentialFingerprint) CredentialObservation`.
  - `StateStore.Load() (PersistentState, RecoveryReport, error)` and `StateStore.WriteThrough(PersistentState) error`.
  - `FenceAllocator.Next() (uint64, error)`.

- [ ] **Step 1: Create the shared deterministic test infrastructure**

Implement `testsupport.Clock` with `Now()`, `Advance(time.Duration)`, and deterministic timer delivery; `testsupport.EventScheduler` with queued events and bounded interleaving enumeration; `FakeHost` and `FakeOpenAI` with scripted calls and call logs; and `CrashController.Hit(name string)` backed by the shared K-point registry. Unit-test each helper without real sleeps or network calls. All later stage suites must import these helpers rather than creating stage-local clocks, hosts, transports, or crash injectors.

- [ ] **Step 2: Write RED tests for identity and credential classification**

Cover same fingerprint, reachable next generation, skipped generation, metadata-only difference, external subject/refresh-token change, chain expiry, ambiguous reconciliation, and old-instance writeback rejection. Include positive and negative tags for INV-28/33/40.

- [ ] **Step 3: Implement the typed epoch and fingerprint model**

Use distinct named integer types to prevent accidental mixing. Hash source components independently and persist only hashes. Keep transition chains ordered, capped at 12 generations and 24 hours.

- [ ] **Step 4: Write RED tests for the four-state SaveAuth WAL**

Inject crashes after `planned`, after host SaveAuth success, and before `applied`; inject explicit failure and unknown outcome. Assert recovery reads current auth and resolves to `applied`, `aborted`, or `CredentialAmbiguous`.

- [ ] **Step 5: Implement WAL ordering**

Expose one method:

```go
func (m *CredentialManager) SaveVersioned(
    ctx context.Context,
    instance AuthInstanceID,
    next HostAuth,
    token ExecutionToken,
) (CredentialSaveResult, error)
```

It must persist `planned` before SaveAuth and persist the terminal phase immediately after a known result. Unknown results remain `outcome_unknown` until reconciliation.

- [ ] **Step 6: Write RED persistence and fence tests**

Test schema migration, future-version read-only mode, temp/fsync/rename ordering through injected filesystem hooks, backup fallback, dual corruption recovery, sensitive-term absence, fence block exhaustion, crashes around ceiling persistence, and restart monotonicity.

- [ ] **Step 7: Implement state store and fence allocator**

Reserve sequence blocks of exactly `1<<20`. Persist and fsync `reserved_ceiling` before issuing any value. On restart, reserve from `persisted_reserved_ceiling+1`; gaps are allowed and regression is not.

- [ ] **Step 8: Add K-point hooks and static discovery**

Mark each write-through, HTTP-before/after, and rename-before/after point with `//kpoint:K_<NAME>`. Extend the gate to enumerate unique K labels and compare them to a checked-in expected list generated by the test.

- [ ] **Step 9: Register §12 group ownership and run the S2 gate**

Name S2's crash/migration cases `TestMockGroupAIdentityPersist` and its identity/security cases `TestMockGroupDIdentitySecurity`. Expose S0's sentinel suite as `TestMockGroupDBoundary`. These are the A/D group implementations, not placeholders for S7.

```powershell
go test ./... -run TestSuiteIdentityPersist -count=1
go test -race ./... -run 'Test(Fence|Credential|StateStore)' -count=1
./scripts/check_refactor_gates.ps1 -Stage S2
go test ./...
```

Expected: all commands exit 0; state-file sensitive scan finds no token, cookie, authorization header, or raw refresh-token value.

- [ ] **Step 10: Commit S2**

```powershell
git add identity.go identity_test.go credential_wal.go credential_wal_test.go state_store.go state_store_test.go fence.go fence_test.go testsupport/ models.go state.go disk_state.go scripts/check_refactor_gates.ps1
git commit -m "feat: add fenced identity persistence primitives"
```

---

### Task 4: S3 — Move the Complete Legacy Refresh Transaction Under the Coordinator

**Files:**
- Create: `coordinator.go`, `coordinator_test.go`
- Create: `auth_transaction.go`, `auth_transaction_test.go`
- Modify: `refresh.go`, `quota.go`, `probe.go`, `main.go`
- Modify: `scripts/check_refactor_gates.ps1`

**Interfaces:**
- Produces:
  - `Intent{Instance, Generation, Class, Source, StartedAfter, Token, Payload}`.
  - `Coordinator.Submit(Intent) Future[OperationResult]`.
  - `Coordinator.PublishSnapshot() *SchedulerSnapshot`.
  - `Coordinator.DrainLegacy(ctx context.Context) DrainReport`.
  - `LegacyRefreshTxn.RunHeld(ctx context.Context, intent Intent) OperationResult`.

- [ ] **Step 1: Write RED coordinator tests**

Test same-instance/op-class/G deduplication, different-instance concurrency, HTTP-slot accounting per request, lock-before-slot order, slot release during propagation waits, causal `started_after`, lease expiry, and discarded stale ExecutionToken results. Use a deterministic event scheduler rather than real goroutines sleeping.

- [ ] **Step 2: Implement the single-owner coordinator loop**

One goroutine owns mutable maps for locks, leases, in-flight operations, and intent queues. Workers receive immutable jobs and return results tagged with ExecutionToken, G, IAE, LoginEpoch, and fingerprint. The owner validates every tag before writeback or success logging.

- [ ] **Step 3: Write RED legacy-envelope coverage**

Instrument every outbound call made by `refreshAuthVersioned`: GetAuth, token refresh, SaveAuth, quota, reset-credits, Probe POST, post-Probe quota, post-Probe reset-credits. Assert one instance lease encloses the full sequence and no nested coordinator intent is submitted.

- [ ] **Step 4: Extract the legacy transaction**

Move the complete sequence into `LegacyRefreshTxn.RunHeld`. Replace internal recursive refresh/probe submissions with direct held-transaction calls. Assign the sole source label `legacy_refresh_txn`.

- [ ] **Step 5: Implement drain and uncertain Probe inheritance**

Stop issuing legacy work, cancel its contexts, join for at most the 2-minute virtual lease, then invalidate unresolved ExecutionTokens. If an unresolved job reached Probe-send phase, persist `SentUnknown` and the full resend-suppression deadline before enabling the new path.

- [ ] **Step 6: Register §12 group ownership and run the S3 gate**

Expose migration/crash cases as `TestMockGroupACoordinatorMigration` and coordinator interleavings, lease recovery, and legacy-envelope races as `TestMockGroupECoordinatorInterleavings`.

```powershell
go test ./... -run TestSuiteCoordinator -count=1
go test -race ./... -run 'Test(Coordinator|LegacyRefresh|Drain)' -count=1
./scripts/check_refactor_gates.ps1 -Stage S3
go test ./...
```

Expected: INV-04/05/09/24/25/26/42/46 positive and negative tags are present and all tests pass.

- [ ] **Step 7: Commit S3**

```powershell
git add coordinator.go coordinator_test.go auth_transaction.go auth_transaction_test.go refresh.go quota.go probe.go main.go scripts/check_refactor_gates.ps1
git commit -m "refactor: coordinate complete refresh transactions"
```

---

### Task 5: S4 — Replace Normal Refresh Timing with Dormant/Active State

**Files:**
- Create: `refresh_controller.go`, `refresh_controller_test.go`
- Modify: `refresh.go`, `dispatch.go`, `config.go`, `config_test.go`
- Modify: `scripts/check_refactor_gates.ps1`

**Interfaces:**
- Produces:
  - `RefreshController.OnPick(now time.Time, snapshot CacheSnapshot) []Intent`.
  - `RefreshController.OnDeadline(now time.Time) []Intent`.
  - `RefreshModeDormant` and `RefreshModeActive`.
  - `UniqueSchedulerSource(initial, staleRecovery, interval bool) IntentSource`.

- [ ] **Step 1: Write RED virtual-clock timeline tests**

Encode the exact design timeline: refresh at 10:00, activity-only at 10:20, interval refresh at 10:30, refresh at 11:00, Dormant at 11:20. Add boundary cases immediately before, at, and after interval and active-window deadlines.

- [ ] **Step 2: Add the source-priority truth table**

Enumerate all eight combinations of initial/stale/interval and assert exactly one source: initial first, stale recovery second, interval third, none otherwise.

- [ ] **Step 3: Implement the controller**

A real pick moves Dormant to Active and extends `last_activity + refresh_active_window`. Aging and `stale_after` never schedule deadlines. Dormant retains accounts, caches, and Management cards and emits no normal-refresh OpenAI request.

- [ ] **Step 4: Remove competing normal-refresh timers**

Delete or disable old stale-only scheduling and any timer that can create a sixth outbound quota source. Keep reset delays and Probe deadlines owned by Probe.

- [ ] **Step 5: Register §12 group ownership, run the S4 gate, and commit**

Expose the refresh timeline, source-priority truth table, Dormant/Active boundaries, and failure isolation cases as `TestMockGroupERefresh`.

```powershell
go test ./... -run TestSuiteRefresh -count=1
./scripts/check_refactor_gates.ps1 -Stage S4
go test ./...
git add refresh_controller.go refresh_controller_test.go refresh.go dispatch.go config.go config_test.go scripts/check_refactor_gates.ps1
git commit -m "refactor: isolate active normal refresh scheduling"
```

Expected: INV-10/11/22/32 tags are complete and all tests pass.

---

### Task 6: S5 — Implement Three-Class Selection, Trial Evidence, and Snapshot Pick

**Files:**
- Create: `selection.go`, `selection_test.go`
- Create: `trial.go`, `trial_test.go`
- Create: `scheduler_snapshot.go`, `scheduler_snapshot_test.go`
- Modify: `scheduler.go`, `scheduler_test.go`, `dispatch.go`
- Modify: `scripts/check_refactor_gates.ps1`

**Interfaces:**
- Produces:
  - `AvailabilityClass` values `Preferred`, `Opportunistic`, `Excluded`.
  - `ClassifyAccount(AccountView, time.Time) AvailabilityClass`.
  - `SelectAccount(SchedulerSnapshot, []Candidate, time.Time) SelectionResult`.
  - `TrialRegistry.TryBegin(AuthInstanceID, now time.Time) bool`.
  - `TrialRegistry.ObserveEvidence(AuthInstanceID, Evidence)`.
  - `atomic.Pointer[SchedulerSnapshot]` used by pick.

- [ ] **Step 1: Create an independent scheduling oracle**

In `selection_test.go`, implement test-only classification and ordering directly from design §5 without calling production helpers. Enumerate all 1,440 single-instance vectors and all `6^N` representative multi-instance vectors for N=1..3, crossed with the three candidates relationships. Register this exhaustive suite as `TestMockGroupB`; S7 only aggregates it.

- [ ] **Step 2: Run RED**

```powershell
go test ./... -run TestSuiteScheduling -count=1
```

Expected: FAIL because production classification/snapshot/trial APIs are absent.

- [ ] **Step 3: Implement classification and ordering**

Scan all Preferred accounts across plugin priorities before scanning any Opportunistic account. Within one confidence class, sort plugin priority descending, then existing monthly mode, expiry, remaining quota, and stable ID. Intersect candidates with the active highest roster tier; never mutate roster from candidates.

- [ ] **Step 4: Implement optimistic trial CAS**

Allow one trial per instance. Beginning a trial atomically excludes the instance and emits one evidence intent. Release only on real usage feedback, reliable quota writeback, or real request success. At 60 seconds retain the trial while evidence is pending; after 5 minutes or 3 retries force TrialUnknown with 1m→2m→5m capped at 15m.

- [ ] **Step 5: Replace pick with snapshot-only selection**

Publish copy-on-write snapshots through `atomic.Pointer[SchedulerSnapshot]`. In `scheduler.pick`, load once, intersect candidates, select, attempt trial CAS when Opportunistic, enqueue optional side effects nonblocking, and return immediately.

- [ ] **Step 6: Run exhaustive, race, and static gates**

```powershell
go test ./... -run TestSuiteScheduling -count=1
go test -race ./... -run 'Test(SchedulerPick|Trial)' -count=1
./scripts/check_refactor_gates.ps1 -Stage S5
go test ./...
```

Expected: INV-12/13/29/41/43/44/45 tags are complete, oracle comparisons pass, and the pick-I/O scan is clean.

- [ ] **Step 7: Commit S5**

```powershell
git add selection.go selection_test.go trial.go trial_test.go scheduler_snapshot.go scheduler_snapshot_test.go scheduler.go scheduler_test.go dispatch.go scripts/check_refactor_gates.ps1
git commit -m "feat: add snapshot scheduling and evidence trials"
```

---

### Task 7: S6 — Replace Legacy Probe with the Dual-Window State Machine

**Files:**
- Create: `probe_state.go`, `probe_state_test.go`
- Create: `probe_classify.go`, `probe_classify_test.go`
- Create: `probe_wal.go`, `probe_recovery_test.go`
- Create: `testdata/probe_classify_golden.json`
- Modify: `probe.go`, `refresh.go`, `state.go`
- Modify: `scripts/check_refactor_gates.ps1`

**Interfaces:**
- Produces:
  - persistent `ProbeWindowState` values exactly matching design §7.2.
  - tagged `ResetBaseline` and `UsageOnlyBaseline`.
  - `ClassifyProbeWindow(ProbeBaseline, QuotaSnapshot, now time.Time) ProbeClassification`.
  - `ProbeController.Advance(instance AuthInstanceID, event ProbeEvent) []Intent`.
  - `ProbeAttempt{AttemptID, Windows, Phase, SendFenceSeq, CreatedAt, VerifyNotBefore, SuppressUntil}`.

- [ ] **Step 1: Generate the independent golden oracle**

Write a test-side generator that produces the approximately 864 Cartesian rows from baseline type, reset offset, window length, usage change, and delay position. Store stable JSON sorted by input tuple. The generator must not import `ClassifyProbeWindow`. Register the complete Probe matrix as `TestMockGroupC`.

- [ ] **Step 2: Add full transition RED tests**

For each of the ten persistent states, enumerate all legal events and representative illegal events. Illegal events must have zero outbound intent and zero persisted mutation. Cross both windows as a 10×10 state product and feed one response to both while asserting independent state transitions.

- [ ] **Step 3: Implement baselines and classification order**

Implement P1–P5 dispatch, the UsageOnly table, and the nine ordered ResetBaseline rules exactly. Establish window length only after two stable adjacent intervals. Unknown lengths use the absolute 120-day guard.

- [ ] **Step 4: Implement WAL-before-send and causal verify**

Allocate `send_fence_seq`, persist/fsync `sending`, then permit HTTP. Persist `sent` immediately after known success. Verify must submit `quota_read` with `started_after=send_fence_seq` and may join only a read whose `read_start_seq` is greater.

- [ ] **Step 5: Implement crash recovery and lease behavior**

Recover `sending` or `sent` as verify-first after `verify_not_before`. Apply `max(verify grace, 10m)` suppression from `created_at`. A `probe_send` lease timeout enters SentUnknown and is never re-signed; read leases may be re-signed and credential writes require fresh auth before re-signing. Register Probe crash/recovery scenarios that belong to §12.A as `TestMockGroupAProbeRecovery`.

- [ ] **Step 6: Remove legacy Probe and transition label**

Delete Probe POST and Probe-after-read behavior from the legacy envelope after the new controller is active. Remove `legacy_refresh_txn` source acceptance where S6 says it ends.

- [ ] **Step 7: Run Probe gate**

```powershell
go test ./... -run TestSuiteProbe -count=1
go test -race ./... -run 'TestProbe' -count=1
./scripts/check_refactor_gates.ps1 -Stage S6
go test ./...
```

Expected: INV-06/07/08/14/15/16/17/18/19/27/36 positive and negative cases pass; golden diff is empty.

- [ ] **Step 8: Commit S6**

```powershell
git add probe_state.go probe_state_test.go probe_classify.go probe_classify_test.go probe_wal.go probe_recovery_test.go testdata/probe_classify_golden.json probe.go refresh.go state.go scripts/check_refactor_gates.ps1
git commit -m "feat: add crash-safe dual-window probe controller"
```

---

### Task 8: S7 — Wire Roster Lifecycle, Capability Branches, Management, and Final Gates

**Files:**
- Create: `roster_controller.go`, `roster_controller_test.go`
- Create: `traceability_test.go`
- Create: `testdata/mock_group_coverage.json`
- Modify: `main.go`, `dispatch.go`, `state.go`, `management.go`
- Modify: `config.go`, `config_test.go`, `management_test.go`, `integration_test.go`
- Modify: `README.md`, `scripts/check_refactor_gates.ps1`

**Interfaces:**
- Produces:
  - `RosterController.Startup`, `WakeForActivity`, `WakeForProbe`, `WakeForManagement`, and `OnSyncResult`.
  - immutable `ActiveRoster{Capability, Confirmed, Provisional, HighestPriority, Generation, Instances}`.
  - final `TestSuiteRosterManagement` and `TestInvariantTraceability`.

- [ ] **Step 1: Write roster lifecycle RED tests**

Cover startup sync, active 5-minute TTL, full idle sleep, Probe pre-wake freshness, Management on-demand sync, concurrent single-flight wakeup, highest-tier atomic replacement, deletion cancellation, Degraded at 30m−ε/30m/30m+ε, FailClosed, recovery, Capability-B WaitingRoster, provisional risk opt-in, and candidates having no roster side effects.

- [ ] **Step 2: Implement Capability-A and Capability-B**

Capability-A uses successful `host.auth.list` with Priority and immediately confirms the highest Codex tier. Capability-B loads last-confirmed as Provisional, keeps normal refresh Dormant, and places Probe windows in WaitingRoster. Only a later authoritative list result confirms the roster. Provisional Probe requires explicit config, age under 4h, fresh GetAuth fingerprint verification, and `provisional:true`.

- [ ] **Step 3: Implement Degraded and FailClosed**

On sync failure keep the last-confirmed set and G unchanged. Until 30 minutes, background requests carry `degraded_roster:true`; after 30 minutes discard background intents and hold Probe in RosterHold while real pick continues from candidates ∩ active tier.

- [ ] **Step 4: Finish Management behavior**

Return only active highest-tier account cards in normal payloads. Preserve cards and cached quota while normal refresh is Dormant. Mark provisional, Degraded, FailClosed, WaitingRoster, CredentialAmbiguous, and risk-option states explicitly. Persist annotation/settings writes before returning success.

- [ ] **Step 5: Add traceability enforcement**

`TestInvariantTraceability` scans all `*_test.go` files for `//inv:INV-01 positive` and `//inv:INV-01 negative` through INV-46. Fail with a sorted missing matrix. The static gate also checks all registered K points and reruns the pick-I/O and sensitive-state scans.

- [ ] **Step 6: Aggregate §12 suites and fill only uncovered cells**

Run the already implemented group suites from S0–S6: A from S2/S3/S6, B from S5, C from S6, D from S0/S2, and E from S3/S4. Generate a machine-readable §12 coverage matrix, identify any scenario row not owned by an existing test, and add only those missing integration cases in `integration_test.go`. Do not recreate clocks, fakes, or oracles in S7. Keep the aggregated run under five minutes using pregenerated golden rows and coverage arrays.

- [ ] **Step 7: Document migration behavior**

Update README: only the highest CPA priority tier loads; equal CPA priorities (preferably 0) are recommended; Capability-B restart defaults Probe to WaitingRoster; `probe_on_provisional_roster` is an explicit risk option; normal refresh sleeps without real requests while Probe remains independent.

- [ ] **Step 8: Run S7 and final acceptance**

```powershell
go test ./... -run TestSuiteRosterManagement -count=1
go test ./... -run TestInvariantTraceability -count=1
go test -race ./...
go vet ./...
./scripts/check_refactor_gates.ps1 -Stage S7
go test ./... -run 'TestMockGroup(A|B|C|D|E)' -count=1
```

Expected: every command exits 0, INV-01–46 each have positive and negative coverage, §12 A–E pass, no K point is missing, no deviation remains open, and the full run stays below the five-minute virtual-time budget.

- [ ] **Step 9: Commit S7**

```powershell
git add roster_controller.go roster_controller_test.go traceability_test.go main.go dispatch.go state.go management.go config.go config_test.go management_test.go integration_test.go README.md scripts/check_refactor_gates.ps1
git commit -m "feat: complete roster lifecycle and invariant gates"
```

---

## Plan Self-Review Checklist

- [ ] Every design section §0–§13 maps to at least one task.
- [ ] S0–S7 order matches the frozen migration table.
- [ ] INV-01–46 are assigned to their stage gates and final traceability.
- [ ] All five outbound refresh source classes are represented; no sixth source is introduced.
- [ ] Capability-A/B, candidates non-authority, pick hot-path constraints, and highest-tier admission are explicit.
- [ ] Credential WAL, fence block reservation, ExecutionToken fencing, and SentUnknown recovery have crash tests.
- [ ] Normal refresh and Probe remain independent.
- [ ] Probe baselines, classification grid, dual-window isolation, suppression, and lease policy are covered.
- [ ] No task modifies CPA source or authorizes remote/release actions.
- [ ] No implementation ambiguity changes the frozen spec; deviations use `docs/deviations.md`.

## Invariant Traceability by Stage

| Stage | Required invariants |
|---|---|
| S0 | INV-01 |
| S1 | INV-31, INV-34, INV-43, INV-44 |
| S2 | INV-03, INV-04, INV-19, INV-23, INV-28, INV-30, INV-33, INV-38, INV-39, INV-40 |
| S3 | INV-04, INV-05, INV-07, INV-08, INV-09, INV-24, INV-25, INV-26, INV-42, INV-46 |
| S4 | INV-10, INV-11, INV-22, INV-32 |
| S5 | INV-12, INV-13, INV-29, INV-41, INV-43, INV-44, INV-45 |
| S6 | INV-06, INV-07, INV-08, INV-14, INV-15, INV-16, INV-17, INV-18, INV-19, INV-27, INV-36, INV-38, INV-39, INV-40 |
| S7 | INV-02, INV-03, INV-20, INV-21, INV-23, INV-31, INV-33, INV-34, INV-35, INV-37 |

The final S7 traceability gate additionally requires positive and negative evidence for every identifier from INV-01 through INV-46, including invariants exercised by more than one stage.
