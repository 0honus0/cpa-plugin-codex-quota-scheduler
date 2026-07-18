# First-Observation Lazy Window Activation Design

**Date:** 2026-07-18

**Scope:** Correct reset-window activation when OpenAI first exposes an unused
quota window with a reset timestamp anchored to the quota-refresh request.

## Problem

OpenAI can return a previously absent five-hour, weekly, or monthly window as:

- `used_percent == 0` (100% remaining); and
- `reset_at ~= observation_time + window_length`.

This is a lazy, not-yet-activated window. In the observed production incident,
three weekly windows were first returned at 22:59:55-22:59:58 immediately after
a manual quota refresh, and their reset timestamps were exactly seven days
after those refresh reads.

The legacy helper `looksLikeLazyReset` recognizes the reset-anchor pattern, but
the S6 `ProbeController` bootstrap path does not use it. The current controller
implements the existing spec literally: every first observation with a future
`reset_at` becomes `WaitingReset`. As a result, the next check is deferred for a
full quota period and no activation request is sent.

## Required Behavior

When automatic reset-window activation is enabled, a newly observed window is a
**suspected first-observation lazy window** only when all of these conditions
hold:

1. `used_percent` is present and equals `0`;
2. the window duration is known;
3. `reset_at` is present; and
4. `abs(reset_at - (observation_time + window_duration)) <= 3 minutes`.

Known duration means:

- five-hour: `limit_window_seconds` when present, otherwise 5 hours;
- weekly: `limit_window_seconds` when present, otherwise 7 days;
- monthly: a positive `limit_window_seconds` is required.

Missing usage, non-zero usage, unknown duration, or a reset timestamp outside
the tolerance keeps the existing `WaitingReset` behavior.

## State Model

Add `SuspectedLazy bool` to `ProbeBaseline`. It is persisted with the existing
Probe window state so restart recovery preserves the reason for the immediate
precheck.

For a newly bootstrapped or reappeared window:

- create a reset baseline with the known `WindowLength`;
- if the strict lazy-window predicate matches, set `SuspectedLazy = true` and
  enter `PendingCheck` immediately;
- otherwise enter `WaitingReset` with the existing reset deadline.

The flag applies only to first observation. Ordinary already-tracked windows
continue using the existing reset-delta classification table.

## Precheck And Verification

For a baseline with `SuspectedLazy = true`, classification occurs before the
ordinary “future reset is not due” rule:

- reset moved beyond the skew tolerance: `ActivatedNew`, clear the flag;
- usage is now non-zero: `ActivatedInferred`, clear the flag;
- same reset anchor and usage remains zero: `StillLazy`;
- invalid or contradictory evidence: preserve the existing retry/anomaly rules.

`StillLazy` follows the existing single-lease sequence:

1. precheck quota;
2. persist the attempt and send fence;
3. send at most one tiny Codex activation request;
4. wait for propagation;
5. verify quota using a read that starts after the send fence; and
6. either confirm activation or enter the existing retry/suppression path.

The fix does not bypass roster authority, credential validation, single-flight,
WAL recovery, resend suppression, or verify-first crash recovery.

## False-Positive Control

The design intentionally does not probe every unused future window. The
combination of explicit zero usage, known duration, and a reset timestamp within
three minutes of `observation_time + duration` isolates the upstream lazy-anchor
pattern.

A genuinely used window normally has non-zero usage or a reset anchor older than
the current observation. If upstream rounds a very small real request down to
zero, the opt-in feature may send one additional tiny request, but the precheck,
single-flight lease, and resend suppression still bound the effect.

## Spec Amendment

Update the first-observation classification table in
`docs/fable-spec/refactor-decision-spec.md`:

- before the current future-reset row, add the strict suspected-lazy case and
  route it to immediate `PendingCheck`;
- retain the existing rule that all other future reset timestamps enter
  `WaitingReset`;
- state that the suspected-lazy marker is persisted and consumed by precheck
  classification before the normal not-due test.

## Tests

Add RED-to-GREEN coverage for:

- first-observation five-hour window at `now + 5h`, zero usage -> immediate
  `PendingCheck`;
- first-observation weekly window at `now + 7d`, zero usage -> immediate
  `PendingCheck`;
- first-observation monthly window with explicit duration, zero usage ->
  immediate `PendingCheck`;
- non-zero usage -> `WaitingReset`;
- missing usage -> `WaitingReset`;
- unknown monthly duration -> `WaitingReset`;
- reset anchor outside the three-minute tolerance -> `WaitingReset`;
- suspected-lazy precheck with unchanged reset/zero usage -> one activation
  send followed by verify;
- precheck with moved reset or non-zero usage -> confirmed without sending;
- concurrent triggers still produce a single activation request;
- persisted suspected-lazy state survives restart and follows verify-first
  recovery rules.

Run the focused Probe tests, the full test suite, the race suite, `go vet`, and
the S7 refactor gate before producing a replacement DLL.
