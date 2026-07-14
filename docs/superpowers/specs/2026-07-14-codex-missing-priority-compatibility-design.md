# Codex Missing-Priority Compatibility Design

## Status

Approved by the user on 2026-07-14.

This document records a post-freeze compatibility decision. It does not edit
the frozen decision spec. `docs/deviations.md` records the corresponding
implementation deviation.

## Problem

CLIProxyAPI 7.2.x exposes roster entries through `host.auth.list` using a
numeric `priority` field with `omitempty`. CPA's routing semantics treat an
absent priority as the default priority `0`, but JSON serialization omits an
explicit zero. The plugin therefore cannot distinguish an absent Codex
priority from an explicitly configured zero by inspecting the callback JSON.

The current implementation treats any roster entry without an explicit
priority as Capability-B. In a normal CPA installation where Codex credentials
use the default priority, startup consequently remains in `WaitingRoster` even
though CPA has provided a usable authoritative Codex roster.

## Decision

Roster authority is scoped to Codex credentials because this plugin is a Codex
scheduler:

- Ignore every non-Codex `host.auth.list` entry before capability and highest-
  tier evaluation.
- Normalize a missing Codex `priority` to an explicit `0`.
- Preserve explicit positive and negative Codex priority values unchanged.
- Continue to require at least one normalized Codex entry.
- Continue to classify callback failure, malformed JSON, missing/null/empty
  `files`, or a roster containing no Codex entries as Capability-B.
- Scheduler candidates remain non-authoritative and cannot create or repair a
  roster.

This supersedes DEV-003 only for provider filtering and missing Codex priority
interpretation. All other DEV-003 conservative fallback cases remain active.

## Implementation Boundary

Apply compatibility normalization at the `ABIHostAuthLister` boundary. That
boundary retains raw-field presence information and is the single adapter from
CPA callback JSON into the plugin's typed `RosterEntry` model.

The adapter will return only Codex `RosterEntry` values. For each returned
entry, `Priority` will always be non-nil: the callback value when present, or a
new pointer to `0` when omitted. `HighestCodexTier`, roster-controller state
transitions, persistence, Probe recovery, and Management projection require no
behavioral changes.

## Error Handling

- Invalid callback JSON remains a decoding error.
- An absent, null, or empty `files` list normalizes to no Codex entries and
  therefore remains Capability-B.
- Entries with an empty or non-Codex provider are ignored.
- The adapter does not consult auth files, scheduler candidates, Management
  state, or previously persisted roster data to invent priority information.

## Tests

Add executable regression coverage proving:

1. A Codex entry whose priority is omitted becomes an explicit priority `0`
   and produces Capability-A.
2. An explicit Codex priority `0` remains `0`.
3. Explicit non-zero Codex priorities preserve highest-tier filtering.
4. Non-Codex entries are ignored even when their priority is omitted.
5. A roster containing only non-Codex entries remains Capability-B.
6. Empty, null, missing, and malformed roster payloads retain their existing
   Capability-B behavior.

Verification must include the focused RED/GREEN test, the S7 refactor gate,
`go test ./...`, `go test -race ./...`, `go vet ./...`, `git diff --check`, and
a Windows `-buildmode=c-shared` DLL build.

## Non-Goals

- No configuration switch for the compatibility behavior.
- No change to CPA auth files or CPA service configuration.
- No use of non-Codex credentials in scheduler, refresh, Probe, or Management
  account views.
- No relaxation of empty-roster or host-callback failure handling.
