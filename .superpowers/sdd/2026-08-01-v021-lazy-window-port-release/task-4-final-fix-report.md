# Task 4 final fix-wave report

Date: 2026-08-01

Branch: `codex/fix-first-observation-probe`

Starting HEAD: `7f66584`

Scope: close every Critical/Important item in `final-review-findings.md`, preserve the reviewed Probe/WAL/concurrency architecture, run fresh local gates, and make no GitHub, tag, release, PR, or Plugin Store mutation.

## Critical 1: disabled opt-in could execute durable Probe work

### Root cause

`enable_reset_probe` was checked during bootstrap but not consistently at production runner admission, `Start`/launch, or the final Probe HTTP start boundary. A durable pending or sent attempt could therefore resume after the setting was disabled. During final self-review, a related scheduler gap was also found: a disabled, expired Probe deadline could still arm the production loop at zero delay even though launches were denied.

### RED evidence

```powershell
go test . -run 'TestProduction(StartDoesNotRunDurablePendingProbeWhenOptInDisabled|ProbeDisableAfterPrecheckFencesCompactPOST|DisabledRestartPreservesSentAttemptWithoutHTTP)$' -count=1 -timeout 60s
```

Actual failures:

- disabled production `Start` issued a quota GET;
- disabling after precheck returned nil and sent the compact POST;
- disabled restart of a sent attempt issued a quota GET.

The final-start gate was mutation-checked by temporarily removing it:

```powershell
go test . -run '^TestProductionProbeDisableAfterPrecheckFencesCompactPOST$' -count=1 -timeout 60s
```

Actual failure: `config-final-gate denial returned nil`; the gate was restored.

The self-review scheduler regression was first observed RED:

```powershell
go test . -run '^TestProductionDisabledProbeDeadlineDoesNotArmRefreshLoop$' -count=1 -timeout 60s
```

Actual failure: `disabled Probe armed refresh loop with delay 0s`.

### GREEN change

- Added a single `resetProbeEnabled` authorization source.
- Gated due/recovery admission, `Start`, launch, Probe deadline selection, and final Probe HTTP starts.
- Held `PluginState.mu.RLock` across the final `host.Do` call so a Management disable linearizes after an already-started request and before any later request.
- Preserved durable sent attempts and send fences while disabled; no verify or resend is attempted until re-enabled.
- Omitted Probe deadlines from production timer selection while the opt-in is disabled, preventing a stale past deadline from creating a zero-delay loop.

### Verification

```powershell
go test . -run 'TestProduction(DisabledProbeDeadlineDoesNotArmRefreshLoop|StartDoesNotRunDurablePendingProbeWhenOptInDisabled|ProbeDisableAfterPrecheckFencesCompactPOST|DisabledRestartPreservesSentAttemptWithoutHTTP)$' -count=1 -timeout 90s
```

Result: PASS (`ok`, 3.367s).

Commit: single final fix-wave commit `fix(probe): close v0.2.1 safety gaps`; exact hash is recorded in the final handoff because this report is included in that commit.

## Important 1: strict authorization used stale baseline duration/kind

### Root cause

`QuotaSnapshot` dropped the current payload's kind and authoritative duration. Strict send authorization therefore reused historical `ProbeBaseline.WindowLength`, allowing a stale 5-hour or long-window baseline to authorize a current payload that had a changed duration, missing Monthly duration, or changed kind.

### RED evidence

```powershell
go test . -run '^TestProductionProbeStrictAuthorizationUsesCurrentWindowEvidence$' -count=1 -timeout 60s
```

Actual failure: all three subtests sent a compact POST:

- changed explicit duration;
- current Monthly duration missing;
- changed long-window kind.

The changed-kind case was strengthened so current Monthly evidence independently satisfies the strict time signature. Temporarily removing only the kind gate then produced:

```powershell
go test . -run '^TestProductionProbeStrictAuthorizationUsesCurrentWindowEvidence/changed_long-window_kind$' -count=1 -timeout 60s
```

Actual mutation failure: one compact POST was sent (`wham/usage -> compact -> wham/usage`). The kind gate was restored.

### GREEN change

- Added `ProbeBaseline.WindowKind`.
- Carried current payload kind, duration, and a duration-known bit through `QuotaSnapshot`.
- Derived current evidence in `probeSnapshots` from the current parsed payload.
- Used only current authoritative duration/kind for strict send authorization; historical baseline length remains scheduling/plausibility data.
- Converted rejected lazy-looking evidence into a bounded read-only observation schedule with no POST.

### Verification

The three production cases pass in the final focused and focused-race suites. The changed-kind mutation fails as described above, proving that the test exercises the kind gate rather than an incidental duration mismatch.

Commit: single final fix-wave commit `fix(probe): close v0.2.1 safety gaps`; exact hash is recorded in the final handoff.

## Important 2: production Probe deadline changes did not reliably wake the timer

### Root cause

Bootstrap/reconciliation and Probe completion could create or advance a Probe deadline without waking the production loop. Roster and AuthBlocked recovery also represented immediate `PendingCheck` work with `Deadline = now` instead of the required zero deadline. A first wake implementation exposed launch self-excitation; the runner needed launch-level coalescing and precise pending-work admission.

### RED evidence

```powershell
go test . -run '^TestProductionProbeTimerWakesDormantLoopForRearmAndRepeatedObservation$' -count=1 -timeout 60s
```

Actual failure: the dormant production timer never ran the re-armed Probe GET.

```powershell
go test . -run 'TestProduction(RosterRecovery|AuthRecovery)ClearsPendingDeadlineAndRunsAutomatically$' -count=1 -timeout 60s
```

Actual failure: both recovered `PendingCheck` windows retained `Deadline == now` instead of a zero deadline.

### GREEN change

- Woke the production loop after authoritative bootstrap/reconciliation deadline changes and after Probe goroutine completion.
- Cleared deadlines on every recovered `PendingCheck` transition.
- Made a running `Start` launch durable pending work.
- Added `probeLaunchActive` launch coalescing so one launch goroutine owns recovery/due execution while later wakeups request a rerun.
- On roster recovery, converted AuthBlocked windows with a newer login to zero-deadline `PendingCheck`.

The production timer test uses the real `Start` loop, proves the authoritative `+30m` deadline before compressing only the wall-clock wait, observes automatic GETs representing `+30m` and `+60m`, asserts zero POSTs, and confirms normal refresh stays Dormant.

No third recurrence of the launch self-excitation failure occurred; the systematic-debugging architecture stop threshold was not reached.

### Verification

Both recovery tests and the production timer test pass in final focused, focused-race, full, and full-race runs.

Commit: single final fix-wave commit `fix(probe): close v0.2.1 safety gaps`; exact hash is recorded in the final handoff.

## Important 3: whole-map persistence could overwrite another instance

### Root cause

Attempt claim, prepared recovery, and failure persistence assigned the controller's whole `ProbeWindows` snapshot. If instance B had committed a durable re-arm but had not yet mirrored it into the controller, an operation for A could replace the entire durable map and lose B.

### RED evidence

```powershell
go test . -run 'Test(ProbeClaimMergesOnlyClaimingInstanceAcrossDurableMirrorBarrier|PreparedProbeRecoveryMergesOnlyFailedInstanceAcrossDurableMirrorBarrier|SentProbeFailureMergesOnlyFailedInstanceAndRestartDoesNotResend)$' -count=1 -timeout 90s
```

Actual failure: all three paths lost durable, intentionally unmirrored instance B state.

### GREEN change

- Attempt claim merges only the claiming instance.
- Prepared recovery merges only each exact prepared instance and conditionally deletes the exact attempt ID/phase.
- Pre-send and sent-failure persistence merge only the failed instance.
- Recovery completion persists only instances touched by recovery intents.
- General controller mirroring now merges controller entries instead of replacing the durable map.
- The only true whole-map replacement remains the global roster hold under `probeHoldMu`.

The sent-failure test additionally proves A's original attempt ID/fence remains unchanged and a disabled restart emits no duplicate compact POST.

### Verification

All three barrier tests pass in final focused, focused-race, full, and full-race runs.

Commit: single final fix-wave commit `fix(probe): close v0.2.1 safety gaps`; exact hash is recorded in the final handoff.

## Important 4: non-strict v0.2.0 migration retained an unbounded deadline

### Root cause

Bootstrap calculated the old deadline before enriching a zero-length baseline. When the current observation was non-strict, the new authoritative length was stored but the old reset-maturity deadline (for example `reset + 7d`) remained.

### RED evidence

```powershell
go test . -run '^TestProbeBootstrapBoundsNonStrictV020MigrationImmediatelyAndAcrossRestart$' -count=1 -timeout 60s
```

Actual failure: nonzero usage, missing usage, and reset-outside-tolerance cases all retained the unbounded reset deadline.

### GREEN change

After authoritative length/kind enrichment, bootstrap immediately recomputes `deadlineFor`. A due result becomes zero-deadline `PendingCheck`; otherwise it receives the observation bound. Non-authoritative roster state still overrides it to zero-deadline `WaitingRoster`. The bounded deadline is persisted so a restart without quota cache preserves it.

### Verification

All three migration cases and their cacheless restarts pass in final focused, focused-race, full, and full-race runs.

Commit: single final fix-wave commit `fix(probe): close v0.2.1 safety gaps`; exact hash is recorded in the final handoff.

## Important 5: arbitrary callback text was persisted in Probe lifecycle logs

### Root cause

Non-status errors were reduced only by token-pattern redaction. Arbitrary upstream body/header/tenant text that did not match a known token pattern was stored in `probe.failed.fields.error` and surfaced through Management endpoints.

### RED evidence

```powershell
go test . -run '^TestProbeLifecycleLogsNeverPersistArbitraryExternalCallbackText$' -count=1 -timeout 60s
```

Actual failure: unique arbitrary sentinels injected through GetAuth, quota GET, compact POST, and verify appeared in Management status/log/export responses.

### GREEN change

Persistent lifecycle failures now contain only stable fields:

- internal `error` code;
- `error_category`;
- optional numeric `http_status`;
- `sent` and existing non-sensitive attempt/window identifiers.

No `err.Error()` callback text is persisted in Probe lifecycle logs.

### Verification

All four callback stages pass against Management status, logs, and export in final focused, focused-race, full, and full-race runs.

Commit: single final fix-wave commit `fix(probe): close v0.2.1 safety gaps`; exact hash is recorded in the final handoff.

## Regression fixture reconciliation

The first full-package run after the production fixes exposed older tests that treated historical `SuspectedLazy` as permanent send authorization or exercised Probe behavior with the opt-in disabled. Those fixtures were updated only to provide the real prerequisites:

- explicit `EnableResetProbe = true` where a Probe path is intentionally exercised;
- current payload kind, authoritative duration, and strict `reset ~= now + duration` evidence where a compact send is expected;
- positive usage at verify where activation is expected.

Their original behavior assertions (authorization/denial, exact request count, state transition, crash recovery, and K-point ownership) were preserved. The focused reconciliation suite passed (`ok`, 8.608s), followed by a full main-package pass (`ok`, 18.844s). Production strict evidence was not weakened.

## Final fresh verification

All commands below were run after the final disabled-deadline fix and changed-kind mutation check were restored.

### Finding-focused tests

```powershell
go test . -run 'Test(ProductionStartDoesNotRunDurablePendingProbeWhenOptInDisabled|ProductionDisabledProbeDeadlineDoesNotArmRefreshLoop|ProductionProbeDisableAfterPrecheckFencesCompactPOST|ProductionDisabledRestartPreservesSentAttemptWithoutHTTP|ProductionProbeStrictAuthorizationUsesCurrentWindowEvidence|ProductionProbeTimerWakesDormantLoopForRearmAndRepeatedObservation|ProductionRosterRecoveryClearsPendingDeadlineAndRunsAutomatically|ProductionAuthRecoveryClearsPendingDeadlineAndRunsAutomatically|ProbeClaimMergesOnlyClaimingInstanceAcrossDurableMirrorBarrier|PreparedProbeRecoveryMergesOnlyFailedInstanceAcrossDurableMirrorBarrier|SentProbeFailureMergesOnlyFailedInstanceAndRestartDoesNotResend|ProbeBootstrapBoundsNonStrictV020MigrationImmediatelyAndAcrossRestart|ProbeLifecycleLogsNeverPersistArbitraryExternalCallbackText)$' -count=1 -timeout 180s
```

Result: PASS (`ok`, 5.287s).

### Finding-focused race tests

The same regex was run with `go test -race . ... -timeout 300s`.

Result: PASS, no race (`ok`, 7.710s).

### Full repository tests

```powershell
go test ./... -count=1 -timeout 300s
```

Result: PASS:

- main package: 21.279s;
- `internal/refactorgate`: 0.591s;
- `testsupport`: 0.675s;
- `scripts/refactor_gates`: no test files.

```powershell
go test -race ./... -count=1 -timeout 300s
```

Result: PASS, no race:

- main package: 26.529s;
- `internal/refactorgate`: 1.649s;
- `testsupport`: 1.828s.

### Static and S7 gates

```powershell
go vet ./...
& .\scripts\check_refactor_gates.ps1 -Stage S7
```

Result: both exit 0. S7 reported roster lifecycle, traceability, K-point, pick-I/O, sensitive-state, and **49 exact Mock row owners passed**.

### Local build and package

```powershell
& .\build.ps1
```

Result: exit 0; its repository test phase passed (main package 19.460s).

The installed GNU Make binary was then used to execute the repository's exact Makefile target because Git Bash has Go but no `make` command:

```powershell
& 'C:\Users\Jeffery\AppData\Local\Microsoft\WinGet\Packages\BrechtSanders.WinLibs.POSIX.UCRT_Microsoft.Winget.Source_8wekyb3d8bbwe\mingw64\bin\mingw32-make.exe' clean package VERSION=0.2.1 GOOS=windows GOARCH=amd64
```

Result: exit 0.

Final local artifacts:

- DLL: `dist/codex-quota-scheduler.dll`, 7,555,072 bytes, SHA-256 `72EAF5560399F3BCA80D1356D91B95795B5B3D03A78028C4C4D1D8BDC1EB0E89`;
- ZIP: `dist/codex-quota-scheduler_0.2.1_windows_amd64.zip`, 3,118,472 bytes, SHA-256 `50adf9273c8f5eb4d5e821019901f8dd0213b28030ef42913d5e280facc264d4`;
- `.sha256` matches the computed ZIP hash;
- ZIP contains exactly `codex-quota-scheduler.dll` (7,555,072 bytes) and no generated header.

## Self-review and concerns

- Lock order remains `rosterMu -> PluginState.mu` at lifecycle/final-HTTP boundaries; no reverse nested acquisition was introduced. Probe run/launch locks are released before loop wake acquisition.
- The final Probe HTTP gate intentionally holds `PluginState.mu.RLock` through `host.Do`. This is required to linearize a Management disable against Probe request start, but a settings write can be delayed for the duration of an in-flight Probe HTTP call.
- Launch self-excitation needed two corrective iterations (`probeLaunchActive` coalescing and pending-only running `Start` launch). It did not recur in focused/full race verification, so no third-failure architectural stop was triggered.
- An initial unqualified `bash` packaging attempt invoked WSL rather than Git Bash and failed after `make clean` because WSL had no executable Go. No tool was installed; the artifacts were restored and the existing GNU `mingw32-make` successfully executed the same Makefile target. Final ZIP contents and checksum were independently verified.
- The protected untracked plan `docs/superpowers/plans/2026-07-13-s7-final-review-fixes.md` was not opened for editing, staged, or committed.
- No GitHub, branch integration, tag, release, PR #3, remote push, or Plugin Store mutation was performed.

## Commit

This report and all scoped production/test changes are intended for one commit:

`fix(probe): close v0.2.1 safety gaps`

The exact commit hash is reported in the final handoff, since this report is itself part of that commit.
