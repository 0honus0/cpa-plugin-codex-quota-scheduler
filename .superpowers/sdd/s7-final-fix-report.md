# S7 final fix report

## Scope

- Enforce the 30-minute degraded-roster deadline autonomously at normal/probe enqueue and final HTTP-start boundaries.
- Restore production scheduler-pick activity wiring without adding synchronous I/O, waits, or unbounded goroutines.

## Root causes

1. RosterFailClosed was derived only by RosterController.finishSync, so a stored RosterDegraded snapshot remained authorized forever when no later roster wake occurred.
2. schedulerPickPublished carried no safe activity callback. The production refreshGlobalRefresherDueSoon helper was therefore dead, and no scheduler pick reached WakeForActivity or QuotaRefresher.OnSchedulerPick.

## TDD evidence

Initial RED command:

    go test . -run 'Test(AutonomousDegradedExpiryAtRequestBoundaries|ProductionSchedulerPickActivityIsAsyncBoundedAndWakesDormantRefresh)$' -count=1

Observed RED:

- Exact 30-minute and +1ns checks still returned normal background allowed=true; final normal HTTP returned nil and started.
- The real ABI scheduler pick returned, but async WakeForActivity was never invoked.

Final focused GREEN:

    go test . -run 'Test(AutonomousDegradedExpiryAtRequestBoundaries|ProductionSchedulerPickActivityIsAsyncBoundedAndWakesDormantRefresh|RefreshSoonDoesNotOverlapRefreshes|ProductionProbePrewakesRosterController)$' -count=1 -timeout 30s
    go test -race . -run 'Test(AutonomousDegradedExpiryAtRequestBoundaries|ProductionSchedulerPickActivityIsAsyncBoundedAndWakesDormantRefresh)$' -count=1 -timeout 60s

## Implementation

- Added RosterController.EnforceLifecycle(now), preserving generation and advancing lifecycle revision once at the exact deadline.
- Injected that authority into the production refresher and derived the effective lifecycle again under the final HTTP-start roster fence.
- FailClosed publication persists Probe WaitingRoster, discards authorization at enqueue, and leaves the published real-pick snapshot usable.
- Successful authoritative recovery advances beyond any concurrent lifecycle revision and restores background work.
- Added an atomic-latest, capacity-one wake, single-consumer pick activity pump.
- Scheduler snapshots publish only the pump's nonblocking enqueue callback plus the matching admission version.
- The consumer invokes WakeForActivity and OnSchedulerPick with the copied request/version/now, activating/extending the refresh window and waking the dormant loop.
- Shutdown atomically removes the global pump/refresher; stale published callbacks become no-ops and no channel is closed under senders.

## Coverage

- Exact degraded boundary: -1ns, at, +1ns.
- No additional roster wake before periodic normal/probe authorization checks.
- Zero HTTP after expiry; persisted Probe WaitingRoster; one FailClosed revision; real pick remains usable.
- Authoritative recovery resumes HTTP.
- ABI pick returns while host roster sync is gated.
- Async host wake, dormant-to-active refresh transition, 256 concurrent picks coalesced to one TTL host list.
- Shutdown during gated activity returns safely; post-shutdown picks do not panic.
- A06 and INV-02/21/35/43 executable ownership updated.

## Verification

- Focused lifecycle/pick/refresh/probe tests: pass.
- go test ./... -count=1 -timeout 180s: pass.
- scripts/check_refactor_gates.ps1 -Stage S7: pass; 49 exact Mock row owners.
- go test -race ./... -count=1 -timeout 300s: pass.
- go vet ./...: pass.
- git diff --check: pass.

## Self-review

- Found and fixed an enqueue regression where an unconditional roster write lock could wait behind an authorized HTTP read fence.
- Found and fixed shutdown retaining an already-stopped global refresher.
- Preserved all pre-existing untracked review artifacts; only this report and the requested code/tests are staged.
- No known remaining blocker.
