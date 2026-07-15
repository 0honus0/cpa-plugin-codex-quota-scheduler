# Management Queue Selection Order Design

## Status

Approved by the user on 2026-07-15.

This document records a post-freeze correction to make the Management account
queue describe the scheduler's real selection order. It does not edit the
frozen decision spec. `docs/deviations.md` records the corresponding deviation
as DEV-012.

## Problem

The production scheduler partitions accounts into `Preferred`,
`Opportunistic`, and `Excluded` before applying plugin priority. Management's
`BuildOrderedAccounts`, however, currently sorts plugin priority before queue
status. A high-priority exhausted account can therefore appear ahead of a
lower-priority account that the scheduler can actually select.

A second precedence mismatch affects the displayed reason. Usage feedback may
leave a durable `TemporaryExhausted` marker while a later successful quota
read authoritatively shows that the LongWindow itself is exhausted. Because
`accountQueueState` checks temporary feedback before validating LongWindow,
Management can label an account with no FiveHour window as "temporarily
exhausted" even though its observed Weekly window is exhausted.

## Decision

Management ordering follows the same availability partition as production
selection:

1. `Preferred` accounts come first.
2. `Opportunistic` accounts come after all `Preferred` accounts.
3. `Excluded` accounts come last.

Plugin priority applies only inside `Preferred` and `Opportunistic`, because
those are the only classes the scheduler may select. Their remaining
tie-breakers stay identical to production selection: Monthly mode, expiry,
remaining quota, then stable auth ID.

Plugin priority is ignored for `Excluded` accounts. They sort by expected
recovery time from earliest to latest, then by stable auth ID. A missing or
zero recovery time sorts after every known recovery time.

This changes presentation order and the legacy ordered diagnostic list. It
does not change authoritative CPA admission, the production pick hot path, or
which account production selection chooses.

## Shared Selection Semantics

Management must derive each account's `AvailabilityClass` through the same
`AccountView` projection and `ClassifyAccount` rules used by the immutable
production scheduler snapshot. The implementation must not create a parallel
approximation of `Preferred`, `Opportunistic`, or `Excluded`.

For selectable classes, Management uses the existing production
`accountViewLess` comparator. This keeps plugin priority meaningful only while
an account can participate in selection and prevents future drift in Monthly,
expiry, remaining-quota, and stable-ID tie-breakers.

For `Excluded`, Management uses the expected recovery time already returned by
the queue-state classifier:

- LongWindow exhaustion: LongWindow reset time.
- FiveHour exhaustion: FiveHour reset time.
- Temporary exhaustion feedback: temporary reset time.
- Open circuit or probe wait: next probe time.
- Auth failure, local failure, stale or unknown quota, missing required window,
  and other states without a reliable recovery time: unknown, therefore last.

## Status Reason Precedence

Authentication, local-state, stale-cache, and circuit safety gates retain their
existing precedence.

After those gates, Weekly and Monthly quota classification proceeds in this
order:

1. Validate the required LongWindow and its reset time.
2. If LongWindow is exhausted, report `weekly_exhausted` or
   `monthly_exhausted`.
3. If durable temporary feedback is still active, report
   `temporary_exhausted`.
4. If FiveHour exists, retain its missing-reset and exhaustion validation.
5. Otherwise report the account available.

The temporary marker is not cleared or rewritten by this precedence change.
It remains effective whenever authoritative LongWindow data does not already
prove the more specific long-window exhaustion reason.

## Management Behavior

- The first card remains the account the scheduler would choose next.
- A lower-priority `Preferred` account appears ahead of every higher-priority
  `Excluded` account.
- `Opportunistic` accounts appear after all normal selectable accounts but
  before hard-excluded accounts.
- Exhausted cards show their actual Weekly, Monthly, FiveHour, or temporary
  reason according to the precedence above.
- The page copy continues to describe plugin priority, but priority no longer
  implies precedence over availability.

## Safety and Compatibility

- Only highest-CPA-priority Codex roster members enter the queue.
- No non-Codex provider behavior changes.
- Missing Codex priority continues to normalize to `0`.
- Missing FiveHour remains valid only when LongWindow is valid.
- Missing or invalid LongWindow remains unavailable and falls back to CPA.
- Scheduler pick remains snapshot-only and performs no I/O.
- No new state, persistence schema, host callback, or OpenAI request is added.

## Tests

Executable coverage must prove:

1. A lower-plugin-priority `Preferred` account sorts ahead of a
   higher-plugin-priority exhausted account.
2. Plugin priority still orders accounts inside `Preferred`.
3. Plugin priority still orders accounts inside `Opportunistic`.
4. `Excluded` accounts ignore plugin priority and sort by known recovery time.
5. Unknown recovery times sort after all known recovery times.
6. A missing-FiveHour account with an exhausted Weekly LongWindow reports
   `weekly_exhausted` even when temporary feedback remains active.
7. The equivalent Monthly case reports `monthly_exhausted`.
8. Temporary feedback still reports `temporary_exhausted` when LongWindow is
   valid and not exhausted.
9. Existing FiveHour exhaustion, missing LongWindow, CPA admission, and
   production snapshot-only pick tests remain green.
10. Browser acceptance shows selectable lower-priority accounts ahead of
    exhausted higher-priority accounts after a real refresh.

Verification includes focused tests, the S7 refactor gate, full tests, race,
vet, diff checking, a Windows DLL build, independent review, deployment to the
real CPA plugin directory, a required CPA service restart, and browser/API
acceptance.

## Non-Goals

- Do not change plugin-priority values or annotation persistence.
- Do not make excluded accounts selectable.
- Do not infer a recovery time that is not already authoritative.
- Do not clear historical temporary feedback solely to improve display text.
- Do not modify CPA's built-in fallback ordering.
