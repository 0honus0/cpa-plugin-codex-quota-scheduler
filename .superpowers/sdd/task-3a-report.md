# Task 3A / S2.5 Report — Production Runtime Wiring and Semantic State-File Migration

## Result

Implemented S2.5 production runtime wiring without extending S3 coordinator/effect/drain behavior. Production now owns separate `.runtime-state.json` and `.user-data.json` files, an authoritative-roster-only `BindingRegistry`, fingerprint genesis, injected `CredentialManager`, and storage-only `ProbeAttemptSeam` persistence.

## TDD RED → GREEN cycles

1. **Semantic paths, migration, Probe seam, genesis/WAL**
   - RED: focused test build failed with undefined `semanticStatePaths`, `loadUserDataWithMigration`, `ProbeAttemptSeam`, and binding registry APIs.
   - GREEN: focused semantic migration, corrupt legacy preservation, Probe restart round trip, genesis, AuthBlocked, and WAL tests passed.
2. **QuotaRefresher dependency injection / Capability-B**
   - RED: `NewProductionQuotaRefresher` was undefined.
   - GREEN: production StateStore/BindingRegistry/CredentialManager injection and Capability-B zero-call test passed.
3. **Migration K-point registry**
   - RED: `TestS2KPointRegistryMatchesSource` reported the four new migration K-points expected but not scanned.
   - GREEN: source/registry scan includes `user_data.go`; S2.5 gate passed.
4. **User-data backup recovery**
   - RED: truncated primary returned a JSON error and did not recover the atomic backup.
   - GREEN: primary/backup atomic write and backup read recovery passed.
5. **Production adapter smoke**
   - RED: smoke hung; root cause was registry write-lock re-entry through adapter lookup during genesis.
   - GREEN: authoritative roster resolves bootstrap identity before registry lookup; deadlock removed.
6. **Injected WAL after genesis**
   - RED: production smoke returned `credential chain missing`; CredentialManager had cached state before genesis committed the chain.
   - GREEN: SaveVersioned refreshes from authoritative StateStore before validating/persisting; adapter-backed SaveAuth smoke passed.

## Migration crash evidence

Shared `testsupport.CrashController` injects and recovers at every boundary:

- `K_USER_MIGRATION_BEFORE_NEW_RENAME`
- `K_USER_MIGRATION_AFTER_NEW_RENAME`
- `K_USER_MIGRATION_BEFORE_LEGACY_RENAME`
- `K_USER_MIGRATION_AFTER_LEGACY_RENAME`

At each point, the test proves either the valid legacy file or retained `.migrated` evidence remains and a clean restart converges to equivalent `.user-data.json` content. New-file precedence, corrupt legacy non-rename, and old/new coexistence are covered.

## Sensitive-state evidence

`TestRuntimeAndUserArtifactsNeverPersistSensitiveValues` scans primary, `.bak`, `.tmp`, and `.corrupt` names for both semantic file families after writing fingerprints derived from raw subject, refresh-token, and Authorization values. No raw sensitive value is present. Runtime persists hashes only; user-data contains configuration/annotations only.

## Genesis / Capability-B evidence

- Positive `//inv:INV-33`: first roster-confirmed observation performs exactly one read-only GetAuth, creates F0, initializes admission/binding/login/token values to 1, creates the credential-chain cursor, and performs no SaveAuth/OpenAI request.
- Negative `//inv:INV-33`: Capability-B produces zero GetAuth/SaveAuth calls; scheduler candidates are not accepted as bindings.
- AuthBlocked remains set through genesis and through primary runtime-state deletion/restart via durable backup.
- Runtime construction failure returns no production runtime and authorizes zero external calls; ABI init leaves background refresher disabled instead of using a legacy fallback.

## Probe seam / future gate

The full requested `ProbeAttemptSeam` shape round-trips across restart. `docs/deviations.md` contains a machine-checkable S6 checklist requiring a strict superset or explicit runtime schema migration and retaining `TestProbeAttemptSeamRuntimeRoundTrip`.

## Gate and verification evidence

- `go test ./... -run TestSuiteRuntimeWiring -count=1` — PASS
- `./scripts/check_refactor_gates.ps1 -Stage S2.5` — PASS
- `./scripts/check_refactor_gates.ps1 -Stage S1` — PASS (`actual=8 baseline=8`)
- `./scripts/check_refactor_gates.ps1 -Stage S2` — PASS
- `go test -race ./... -run 'Test(SuiteRuntimeWiring|Runtime|Binding|ProbeAttempt|UserData|CredentialManager)' -count=1` — PASS
- `go test ./...` — PASS
- `go vet ./...` — PASS
- `git diff --check` — PASS (Git emitted only existing LF→CRLF working-copy notices)

## Files

Created:

- `binding_registry.go`
- `runtime_host_adapter.go`
- `runtime_state.go`
- `user_data.go`
- `runtime_wiring_test.go`
- `.superpowers/sdd/task-3a-report.md`

Modified:

- `state_store.go`, `credential_wal.go`, `refresh.go`, `main.go`, `dispatch.go`
- `management.go`, `management_test.go`, `kpoints_test.go`
- `scripts/check_refactor_gates.ps1`, `scripts/refactor_gates/s2-kpoints.txt`
- `README.md`, `docs/deviations.md`

## Self-review

- No EffectJournal, coordinator result application, drain/lifecycle, full S3 fencing, or S6 probe transition logic was added.
- Runtime/user files never share a transaction and production management reads/writes only `.user-data.json` after migration.
- The runtime constructor is fail-closed on persistence failure.
- Genesis is roster-authoritative and read-only before WAL-backed SaveAuth is explicitly invoked.
- New K-points have source labels, runtime hits, shared crash coverage, and checked registry entries.

## Concerns / later-stage ownership

- The production refresher captures the startup roster snapshot. Capability-B→A recovery/synchronization remains explicitly owned by S7 and is not implemented here.
- The existing incomplete S3 seam remains untouched and unapproved, as required.
