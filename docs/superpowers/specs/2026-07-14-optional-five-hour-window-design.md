# Optional FiveHour Window Design

## Status

Approved by the user on 2026-07-14.

This document records a post-freeze compatibility decision. It does not edit
the frozen decision spec. `docs/deviations.md` records the corresponding
implementation deviation as DEV-010.

## Problem

OpenAI usage responses may temporarily omit the general FiveHour quota window
while still returning a valid Weekly or Monthly LongWindow. The parser already
accepts a secondary-only response, but Weekly scheduling currently treats a
missing FiveHour window as invalid. As a result, accounts with authoritative
long-window quota are classified unavailable and unnecessarily fall back to
CPA.

Historical durable Probe state adds a second concern. Once a successful quota
read confirms that FiveHour is absent, stale FiveHour windows must not keep
triggering sends. However, crash recovery must retain any missing window still
referenced by a nonterminal durable Probe attempt until that attempt reaches a
terminal state.

## Decision

- FiveHour is optional when a valid Weekly or Monthly LongWindow exists.
- A valid LongWindow requires a recognized Weekly or Monthly family and a
  non-zero reset time.
- If LongWindow is absent or invalid, the account remains Unknown/unavailable
  and scheduler selection falls back to CPA.
- Never synthesize a FiveHour window, reset time, usage percentage, or Probe
  baseline.
- When FiveHour exists, preserve all current reset and exhaustion validation.
- Weekly and Monthly accounts with a valid, non-exhausted LongWindow and no
  FiveHour window are schedulable and sort by the LongWindow reset time.
- Management hides the FiveHour row when the window is absent and continues to
  show the LongWindow row.

## Parser and Scheduler Boundaries

`ParseCodexUsagePayload` remains the source of truth for observed windows. A
secondary-only `rate_limit` response produces `FiveHour == nil`, a populated
`LongWindow`, and the family inferred from the long window duration. Explicit
regression coverage will lock this behavior.

`accountQueueState` validates LongWindow first for both Weekly and Monthly
families. It validates and evaluates FiveHour only when non-nil. Unknown family,
missing LongWindow, zero LongWindow reset, or exhausted LongWindow remain
unavailable.

## Probe Lifecycle

Probe bootstrap continues to create state only for quota windows actually
present in a successful observation. It must not create FiveHour state when
`ParsedQuota.FiveHour` is nil.

After a successful quota response, Probe reconciliation compares observed
windows with persisted/controller state for the same active auth instance:

1. Present windows are bootstrapped or updated through existing behavior.
2. An absent FiveHour window is removed from controller and persistence when no
   nonterminal durable Probe attempt references `ProbeWindowFiveHour`.
3. If a nonterminal attempt references FiveHour, preserve that window until
   recovery completes the attempt. Cleanup then occurs during recovery or the
   next successful refresh.
4. If FiveHour later reappears, normal bootstrap recreates its Probe state from
   the newly observed quota window.

The cleanup is per-window. It must not delete LongWindow state, the auth
instance, unrelated attempts, or another account's state.

## Error Handling and Safety

- A malformed response remains a refresh failure and cannot prove window
  absence.
- Only a successful parsed quota response can trigger missing-window cleanup.
- Missing FiveHour does not clear auth failures, local failures, staleness,
  circuit state, temporary exhaustion, or LongWindow exhaustion.
- Nonterminal attempt phases are `Prepared`, `Sending`, `Sent`, and
  `SentUnknown`; these retain every referenced missing window.
- Persistence failure leaves the previous Probe state intact or surfaces the
  existing error path; it must not publish partial cleanup as successful.

## Management Behavior

The JSON status model may continue returning a `five_hour` object marked
missing for compatibility. Both server-rendered and dynamic account-card paths
must omit the FiveHour quota row when `missing` is true. The LongWindow row
remains visible. No fake zero-percent FiveHour bar is permitted.

## Tests

Executable coverage must prove:

1. A secondary-only Weekly payload parses with nil FiveHour and a valid Weekly
   LongWindow.
2. A secondary-only Monthly payload parses with nil FiveHour and a valid
   Monthly LongWindow.
3. Weekly and Monthly accounts without FiveHour remain schedulable when their
   LongWindow is valid.
4. Missing or invalid LongWindow remains unavailable and uses CPA fallback.
5. Existing FiveHour exhaustion and missing-reset rules remain unchanged.
6. Probe bootstrap creates no absent FiveHour state.
7. Successful refresh removes historical FiveHour Probe state when safe.
8. A nonterminal durable attempt preserves referenced FiveHour state until
   recovery or a later refresh can safely clean it.
9. Reappearing FiveHour data recreates normal Probe state.
10. Management omits the absent FiveHour row and keeps the LongWindow row.

Verification includes focused tests, the S7 refactor gate, full tests, race,
vet, diff checking, a Windows DLL build, and browser acceptance against the
installed plugin.

## Non-Goals

- No configuration switch for requiring FiveHour.
- No inference from GPT-4o, GPT-4, code-review, or additional rate limits.
- No fake quota window or reset time.
- No relaxation when LongWindow is missing or invalid.
- No change to CPA fallback semantics.
