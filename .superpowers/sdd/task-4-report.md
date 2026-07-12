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
- The legacy body still performs its existing admission-version-guarded state mutations while held. Coordinator result validation fences completion and success disposition, but the S3 seam does not retrofit S2 identity fields into legacy `AccountState`; that broader identity/state-machine integration belongs to the later snapshot/state-machine stages.
- SentUnknown inheritance is exposed and tested at the coordinator callback boundary. Durable Probe WAL/state persistence remains owned by S6; S3 does not introduce a second persistence authority.
