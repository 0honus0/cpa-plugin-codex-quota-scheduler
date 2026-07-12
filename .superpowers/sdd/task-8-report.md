# Task 8 / S7 report

## RED / GREEN

- RED: lifecycle suite initially failed to compile because `ActiveRoster`,
  `RosterController`, TTL/degraded boundaries, and wake APIs did not exist.
- GREEN: startup, exact 5-minute freshness cutoff, idle/no-timer behavior,
  Probe and Management wakeups, same-instant singleflight, highest-tier atomic
  publication, candidate non-authority, and B-to-A recovery pass.
- RED: `probe_on_provisional_roster` config fields were absent.
- GREEN: defaults false and round-trips through YAML and Management settings.
- RED: provisional Probe had no age/fingerprint/risk gate.
- GREEN: under-four-hour provisional snapshots require explicit opt-in and a
  positive verification callback; expired snapshots are discarded.
- RED: production Probe did not pre-wake roster synchronization.
- GREEN: `RunProbeDueOnce` now wakes the sole global controller first.

## Lifecycle timelines

- Startup: Capability B/WaitingRoster -> authoritative list -> filter highest
  Codex priority -> publisher/genesis succeeds -> atomic Capability A publish.
- Active: wakes inside `[last sync,last sync+5m)` reuse the immutable snapshot;
  exact `+5m` synchronizes. No controller timer exists while fully idle.
- Probe/Management: Probe pre-wakes before window processing; Management forces
  an on-demand sync. Concurrent same-instant requests join one host call.
- Replacement: publisher success advances generation and exposes the full new
  tier; publisher failure retains the old tier. Removed IDs are supplied to the
  cancellation callback only after successful publication.

## Degraded boundaries

- `< confirmed+30m`: last confirmed instances and generation are retained,
  health is `Degraded`, and background intent is allowed with the degraded
  marker represented by `Health`.
- `== confirmed+30m` and `> confirmed+30m`: health is `FailClosed`, background
  work is disabled, while the retained instance set remains available for the
  existing real-pick intersection path.
- A later authoritative success clears degradation and advances generation.

## Capability B and provisional behavior

- Failed/unavailable authoritative list yields Capability B `WaitingRoster`.
- Candidates are never read as roster truth.
- Provisional snapshots are immutable copies, expire at four hours, never
  become confirmed, and permit Probe only with both the explicit risk setting
  and fresh fingerprint verification.

## Management / security

- Existing active-tier snapshot ordering remains the account-card source, so
  lower CPA tiers do not enter normal payloads and Dormant cached cards remain.
- Settings now include `probe_on_provisional_roster`.
- Settings persistence is write-through: the disk image is saved before the
  in-memory config is replaced or success is returned.
- Existing Resource/Management authentication boundary and annotation
  persistence paths are preserved.

## Traceability and static gates

- `TestInvariantTraceability` scans every root `*_test.go` and requires sorted
  positive and negative tags for INV-01 through INV-46.
- S7 gate reruns zero pick-I/O closure, S2/S6 K-point registries, lifecycle,
  traceability, candidate-negative, B-to-A, and Mock A-E suites.

## Mock ownership / gaps

- A: auth transaction, identity persistence, Probe recovery, typed intent.
- B: selection.
- C: Probe state product.
- D: boundary and identity security.
- E: coordinator, refresh, typed interleavings.
- Machine-readable matrix: `testdata/mock_group_coverage.json`.
- Uncovered rows: none; no replacement clocks, fakes, or oracles were added.

## Verification

- `go test ./... -run TestSuiteRosterManagement -count=1`: pass.
- `go test ./... -run TestInvariantTraceability -count=1`: pass.
- `go test -race ./...`: pass (20.7s command time).
- `go vet ./...`: pass after removing a self-assignment reported by vet.
- `./scripts/check_refactor_gates.ps1 -Stage S7`: pass.
- `go test ./... -run 'TestMockGroup(A|B|C|D|E)' -count=1`: pass.
- Full `go test ./... -count=1`: pass (under ten seconds, virtual-time budget).

## Files

- Added: `roster_controller.go`, `roster_controller_test.go`,
  `traceability_test.go`, `testdata/mock_group_coverage.json`.
- Modified: `main.go`, `dispatch.go`, `probe_runtime.go`, `config.go`,
  `config_test.go`, `management.go`, `README.md`,
  `scripts/check_refactor_gates.ps1`.

## Self-review / concerns

- The controller intentionally has no autonomous ticker; every host call is
  attributable to startup, activity, Probe, or Management.
- `Candidates` remains in the option shape only as a negative-test seam and is
  never consulted.
- The production provisional verification callback is not installed until a
  host GetAuth fingerprint adapter is available; therefore the risk option is
  fail-closed in production rather than bypassing verification.
