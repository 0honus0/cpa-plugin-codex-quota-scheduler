# Task 4 / S3 report

## Status

Implemented the S3 legacy coordinator migration seam. `refreshAuthVersioned` now submits one `legacy_refresh_txn` intent and waits for its future. `LegacyRefreshTxn.RunHeld` executes the pre-S4 legacy body under one instance lease. A held host wrapper accounts for each GetAuth, SaveAuth, and HTTP request separately and does not submit nested intents.

## RED / GREEN cycles

1. RED: `go test ./... -run TestSuiteCoordinator -count=1` failed to compile because `Coordinator`, `Intent`, `HeldLease`, operation classes, futures, and the source tag did not exist.
   GREEN: added the owner loop, same-key deduplication, per-instance leases/queues, cross-instance execution, per-request slots, futures, virtual lease timer injection, result fencing, snapshots, and drain entrypoint. Focused suite passed.
2. RED: complete-envelope test initially failed to compile at the new held transaction boundary (`time` dependency missing).
   GREEN: the exact sequence passed: GetAuth, token refresh POST, SaveAuth, quota GET, reset-credits GET, Probe POST, post-Probe quota GET, post-Probe reset-credits GET.
3. RED: drain test returned zero cancellation/invalidation/SentUnknown counts because the result could win before the drain command was observed.
   GREEN: made cancellation observation deterministic, marked draining executions stale, joined their result, inherited uncertain Probe state through the callback, and returned counts.

No real sleeps are used by the new coordinator tests. Lease expiry is driven by an injected virtual clock.

## Outbound-call coverage

- GetAuth: held host slot
- token refresh: held host HTTP slot
- SaveAuth: held host slot
- quota: held host HTTP slot
- reset-credits: held host HTTP slot
- Probe POST: held host HTTP slot; send phase is marked before dispatch
- post-Probe quota: held host HTTP slot
- post-Probe reset-credits: held host HTTP slot

The envelope has one coordinator intent and one instance lease. Internal calls remain direct held calls.

## Drain scenarios

- Normal completion: joined and future completed.
- Cancellation: context is cancelled and the returning execution is invalidated.
- Uncertain Probe: Probe send phase and full ten-minute suppression deadline are captured; stale completion invokes SentUnknown inheritance callback.
- Lease expiry: late ExecutionToken result is discarded and not reported as applied.

## Mock ownership

- Group A: `TestMockGroupACoordinatorMigration`
- Group E: `TestMockGroupECoordinatorInterleavings`

## Verification

- `go test ./... -run TestSuiteCoordinator -count=1` — pass
- `go test -race ./... -run 'Test(Coordinator|LegacyRefresh|Drain)' -count=1` — pass
- `./scripts/check_refactor_gates.ps1 -Stage S3` — pass
- `go test ./... -count=1` — pass
- `go vet ./...` — pass after avoiding a mutex-copy in the transaction clone
- `git diff --check` — pass (Git reports only LF/CRLF conversion warnings)

## Files

- `coordinator.go`, `coordinator_test.go`
- `auth_transaction.go`, `auth_transaction_test.go`
- `refresh.go`
- `scripts/check_refactor_gates.ps1`

`quota.go`, `probe.go`, and `main.go` required no direct edit: quota/probe outbound calls are intercepted through the held HostClient, and all construction paths (including `main.go`) use the updated `NewQuotaRefresher` constructor.

## Self-review and concerns

- The mutable coordinator maps are owned only by the coordinator loop.
- Deduplication key is instance/class/generation.
- Instance lock is acquired before any HTTP slot; slots are acquired per request and released immediately after it.
- The S1 pick ratchet remains unchanged.
- S0 Management code is unchanged.
- The legacy body now mutates only a transaction-local `PluginState` clone. It returns an effect journal; the owner validates Instance/IAE/G/LoginEpoch/fingerprint and applies account effects and buffered logs only while the binding remains current.
- SentUnknown inheritance persists the approved minimal `ProbeAttemptSeam` through the authoritative runtime `StateStore`; S6 still owns the later Probe transition machine.

## Review correction wave (post-S2.5)

### RED / GREEN evidence

1. RED: `TestCoordinatorNonCooperativeExpiryReleasesLease` hung because expiry only cancelled context and waited forever for the worker. GREEN: expiry now terminalizes the future, releases lease/inflight ownership, advances the conservative dependency barrier, starts eligible queued work, and ignores the late result.
2. RED: `TestCoordinatorDrainCancelsQueuedWithoutStarting` reported one completed job and could start queued work during drain. GREEN: queued jobs are removed and cancelled without execution; `tryStart` is disabled after stop-issue; reports count jobs.
3. Existing admission-race regressions failed after introducing the overlay because its admission snapshot remained current locally. GREEN: every held outbound operation revalidates the live admission and full binding immediately after slot acquisition.
4. Added stale-component coverage for Instance, IAE, G, LoginEpoch, and fingerprint. Every case returns `discarded_stale`, applies no journal, and emits no buffered host log.
5. Added concurrent Close/Submit/PublishSnapshot coverage. Lifecycle serialization prevents sends into an owner loop that has exited.
6. Added durable legacy SentUnknown restart coverage. Phase, send fence, and suppression deadline survive a fresh `StateStore` load.

### Corrected ownership

- `LegacyRefreshTxn.RunHeld` requires exactly one non-nil held lease.
- Worker state/log changes are isolated in `LegacyEffectJournal`.
- Coordinator validation uses the authoritative `BindingRegistry` and the complete writeback tuple.
- Token refresh SaveAuth uses the shared `CredentialManager` WAL, with binding validation before and after the held SaveAuth call.
- GetAuth, SaveAuth, quota/reset/Probe HTTP, and post-validation host Log callbacks consume the global request slot.
- Probe send is marked only after acquiring its request slot, immediately before calling the host.
- Non-cooperative expiry and virtual drain deadline persist uncertain Probe state immediately and unblock the instance.
- Impossible `StartedAfter` dependencies resolve conservatively when a predecessor expires or is discarded.
- Nil coordinator paths fail closed; the direct legacy fallback was removed.

### Fresh verification

- coordinator/focused suite: pass
- targeted race suite: pass
- S1 gate: pass (`actual=3`, baseline allowance `8`)
- S2 gate: pass
- S2.5 gate: pass
- S3 gate: pass
- full `go test ./... -count=1`: pass
- `go vet ./...`: pass
- `git diff --check`: pass with line-ending warnings only

### Remaining concern

SaveAuth is irreversible once dispatched. If admission changes while the host call itself is executing, the credential WAL records the outcome, while post-call fencing prevents any stale PluginState effect or success log. This is the strongest safe guarantee available without host-side CAS and matches the S2 WAL outcome protocol.
