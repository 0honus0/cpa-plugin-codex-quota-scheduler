# S7 final-fix Task 3 report

## Scope and outcome

Implemented persisted provisional Probe recovery end to end from approved base `212d7e5`.

- Authoritative roster publication writes through a last-confirmed snapshot containing only account IDs, auth indexes, priorities, durable generation, confirmation time, and the four credential fingerprint hashes.
- Capability-B restart reconstructs an immutable `Provisional`/`WaitingRoster` snapshot but never marks it confirmed and makes no GetAuth, SaveAuth, or OpenAI call while the risk option is off.
- `QuotaRefresher.ProvisionalRoster()` validates age, duplicate/empty metadata, and fingerprint-hash integrity before returning recovery state.
- `QuotaRefresher.VerifyProvisionalRoster(context.Context, ActiveRoster)` requires exact persisted roster metadata, matching durable bindings, and a fresh GetAuth fingerprint for every involved instance. Missing auth, host error, parse failure, component mismatch, binding mismatch, cancellation, or age `>= 4h` clears authorization and returns false.
- Successful verification grants a one-use provisional permit consumed only by `RunProbeDueOnce` or `RunProbeRecoveryOnce`; normal refresh remains disallowed.
- Production `main.go` passes persisted provisional state and the verifier into `RosterController`. Explicit risk mode may start the dormant Probe loop; default-off cannot start it.
- Every provisional OpenAI precheck/send/verify request continues through `doBackgroundHTTPRequest(..., true)` and carries `X-CPA-Roster-Lifecycle: provisional`.
- A later failed verification revokes and publishes `BackgroundAllowed=false`, returning the runtime to `WaitingRoster`.

## RED evidence

1. Initial focused production tests:

   `go test ./... -run 'TestProductionProvisional(Recovery|Verification|RequestMarker)' -count=1`

   Failed to build with the expected missing production APIs:

   - `r.ProvisionalRoster undefined`
   - `r.VerifyProvisionalRoster undefined`

2. Revocation publication regression:

   `go test ./... -run TestProvisionalVerificationFailureRevokesPreviouslyPublishedProbeAccess -count=1`

   Failed because the controller retained an observed `BackgroundAllowed:true` publication after the next verification returned false.

3. Corrupt fingerprint metadata regression:

   `go test ./... -run TestProductionProvisionalRecoveryRejectsCorruptFingerprintMetadata -count=1`

   Failed because a semantic snapshot with only a forged composite hash was reconstructed as provisional.

4. Production risk-start regression:

   `go test ./... -run TestProductionProvisionalRecoveryRiskStartRunsVerifiedProbe -count=1`

   Failed after the explicit two-second deadline with zero requests because Capability-B startup could not start the verifier-owned Probe loop.

5. Mixed-priority semantic corruption regression:

   `go test ./... -run TestProductionProvisionalRecoveryRejectsCorruptFingerprintMetadata/mixed_priorities -count=1`

   Failed because a forged persisted snapshot containing two different priorities was reconstructed. Recovery now requires every persisted entry to belong to one identical highest-priority tier.

6. Review-driven credential TOCTOU and live-config regressions:

   `go test ./... -run 'TestProductionProvisionalVerification(RechecksActualPrecheckFingerprint|TracksRiskConfig)' -count=1`

   Initially failed to build because the actual-precheck mismatch error and configured verifier did not exist. The actual Probe GetAuth result is now fingerprint-checked before OpenAI, and the verifier reads the live risk option.

7. Disable-versus-verifier commit race:

   `go test ./... -run TestProvisionalRiskDisableFencesInFlightVerificationCommit -count=1`

   Initially failed to build because the controller lacked an atomic provisional commit guard. Production now holds the PluginState config read lock across the final enabled check and controller commit; disable waits for or precedes that commit and then synchronously revokes.

## GREEN and security evidence

- Focused Task 3 selection:

  `go test ./... -run 'TestProductionProvisional(Recovery|Verification|RequestMarker)' -count=1`

  PASS; main package `2.016s`.

- Provisional, Capability-B zero-call, sensitive-state, and revocation selection:

  `go test ./... -run 'Test(ProductionProvisional|ProductionRefresherCapabilityBActualPathMakesZeroCalls|RuntimeAndUserArtifactsNeverPersistSensitiveValues|StateStoreContainsNoSensitiveTerms|ProvisionalVerificationFailureRevokesPreviouslyPublishedProbeAccess)' -count=1`

  PASS; main package `2.111s`.

- Full suite:

  `go test ./... -count=1`

  Final fresh PASS; main package `6.221s`, refactor gate `0.588s`, testsupport `0.690s`.

- Race detector:

  `go test -race ./... -count=1`

  Final fresh PASS; main package `10.942s`, refactor gate `1.647s`, testsupport `1.760s`.

- Static analysis:

  `go vet ./...`

  PASS with exit code 0 and no diagnostics.

- Diff hygiene:

  `git diff --check`

  PASS with exit code 0. The diff is limited to Task 3 production/test files plus the required fingerprint-integrity and controller-revocation support. Existing unrelated untracked SDD artifacts were preserved.

## Requirement mapping

- INV-02 / INV-34: exact highest-tier persisted metadata; explicit risk-only provisional exception; all three OpenAI requests carry the provisional marker; mismatch/missing/error paths issue zero OpenAI calls.
- INV-20: provisional verification additionally requires the currently durable binding to match persisted ID/index/fingerprint, preventing removed or replaced bindings from inheriting authorization.
- INV-30: persisted recovery contains hashes only; raw access/refresh secrets are absent; malformed hash components/composite fail closed; existing sensitive-artifact scans pass.
- INV-35: default-off, expired, failed, missing, corrupt, and mismatched provisional states stay `WaitingRoster` with zero background OpenAI requests.
- INV-37: last-confirmed roster publication uses mirrored write-through persistence before runtime/scheduler publication.

## Independent review

The final independent review raised two Important authorization findings: a fingerprint TOCTOU between verification and the actual Probe precheck, and a runtime config-toggle race. Both were fixed and re-reviewed. The reviewer confirmed both resolved, found consistent lock ordering, and reported no new Critical or Important findings.

## Diff summary

Production changes are in `state_store.go`, `identity.go`, `refresh.go`, `probe_runtime.go`, `roster_controller.go`, `management.go`, and `main.go`. Tests are in `runtime_wiring_test.go`, `probe_runtime_test.go`, and `roster_controller_test.go`.

No frozen-spec deviation was required. The optional `last_confirmed_roster` field remains backward-compatible under state schema 1; absent or semantically invalid data fails closed without rewrite, while JSON corruption continues through the existing primary/backup/quarantine recovery path.

## Follow-up authorization hardening after commit 138884b

The main Task 3 review identified four additional Important boundary gaps. Each was reproduced before production changes.

### Follow-up RED evidence

Command:

`go test ./... -run 'TestProductionProvisionalRecoveryRejectsCorruptFingerprintMetadata/future_confirmation|TestCapabilityBProvisionalProbeRequiresRiskOptionAgeAndFingerprint|TestProductionProvisionalVerificationRejectsCorruptBindingIdentity|TestProductionProvisionalRequestExpiryDuringPrecheckReturnsWaitingRoster|TestProductionProvisionalConfigDisableLinearizesWithHostStart' -count=1`

Observed failures:

- a future `ConfirmedAt` reconstructed in both StateStore recovery and controller construction;
- exact-expiry during the actual precheck completed with `err=<nil>` and reached OpenAI;
- config disable completed while `host.Do` was already at its start boundary;
- corrupt AuthID, zero/wrong Instance, and stale Generation bindings all verified.

### Follow-up fixes

- Added one shared `provisionalAgeValid` predicate implementing exactly `0 <= now-confirmed_at < 4h`; recovery, controller construction/wake, configured startup, lifecycle observation, permit authorization, and final outbound authorization use it.
- Every provisional outbound OpenAI call rechecks strict age. Expiry between verification and precheck/send/verify returns `ErrCapabilityB`, clears the permit, revokes runtime access, and durably moves Probe windows to `WaitingRoster` with no deadline.
- The provisional final gate now holds the same PluginState config read lock through `host.Do`. A settings writer therefore linearizes either before the request (denied) or after the request start/completion; it cannot commit disabled state between the final authorization check and host start.
- Durable verification now requires the binding key and value to agree on AuthID, the nonzero expected legacy Instance, binding Generation equal to provisional Generation, AuthIndex equality, and the exact four-component fingerprint.
- Existing tests that manually constructed provisional authorization now include the explicit risk option and a valid confirmation time.
- Review found one final state-only race after the first follow-up: age could cross four hours during GetAuth verification before permit/controller publication. `TestProductionProvisionalVerificationExpiryDuringGetAuthIssuesNoPermit` and `TestProvisionalVerificationCannotCommitAfterAgeExpires` were observed RED. Verification now rechecks age after all GetAuth reads, the configured wrapper rechecks live config and age, and the controller commit rechecks age inside its atomic commit. Both regressions are GREEN.

Lock order is lifecycle roster read lock → config read lock → `host.Do`. Config writers release their write lock before controller/lifecycle callbacks; the provisional commit guard holds only config read lock → controller lock and releases it before observer notification. The race detector found no inversion or data race.

### Follow-up GREEN evidence

- Final expanded provisional/security selection: PASS; main package `2.621s`.
- Final full `go test ./... -count=1`: PASS; main package `6.603s`, refactor gate `1.116s`, testsupport `1.362s`.
- Final full `go test -race ./... -count=1`: PASS; main package `11.661s`, refactor gate `1.732s`, testsupport `1.883s`.
- `go vet ./...`: PASS, no diagnostics.
- `git diff --check`: PASS.

The final independent re-review reports no remaining Critical or Important findings.
