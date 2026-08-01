# v0.2.1 Lazy-Window Observation And Release Design

**Date:** 2026-08-01

**Scope:** Port the useful behavior from GitHub PR #3 onto the locally reviewed
Probe architecture, preserve its recovery and concurrency guarantees, and
release the result as v0.2.1.

## Context

The local branch `codex/fix-first-observation-probe` already fixes the original
incident in which a first observation reported zero usage and
`reset_at ~= observed_at + window_length` but no activation request ran. It
adds strict first-observation evidence, immediate production wake, visible
Probe lifecycle logs, atomic attempt/window completion, instance-scoped
persistence, coalesced cross-account wakeups, and verify-first restart safety.

GitHub PR #3 independently fixes the same first-observation symptom and adds
three valuable behaviors that are absent locally:

1. upgrade v0.2.0 baselines that lack an authoritative window length;
2. re-arm a `Confirmed` window from a later authoritative observation; and
3. perform opt-in read-only Probe observations while normal refresh is dormant
   so an OpenAI compensation, promotional reset, or other external reset can be
   discovered.

PR #3 cannot be merged verbatim. It is based on the earlier v0.2.0 state and
retains the non-atomic terminal ordering that the local branch has since fixed.

## Selected Architecture

Port the behaviors conceptually, not commit-for-commit. The local Probe state
machine, `SuspectedLazy` evidence marker, coordinator lease, WAL, send fence,
atomic terminal completion, instance-local merge, and coalesced rerun remain
authoritative.

The alternative approaches are rejected:

- merging PR #3 directly would regress crash recovery and cross-instance
  persistence;
- keeping only the current local fix would miss later server-side resets while
  an account remains unused;
- restoring ordinary quota refresh while dormant would violate the zero-normal-
  refresh dormant contract.

## Authoritative Window Evidence

A window length is authoritative when it comes from the current quota payload:

- FiveHour: positive `limit_window_seconds`, otherwise the established 5-hour
  protocol fallback;
- Weekly: positive `limit_window_seconds`, otherwise the established 7-day
  protocol fallback;
- Monthly: positive `limit_window_seconds` is required.

Zero usage must be explicit. Missing `used_percent` is not converted to zero and
cannot authorize immediate pre-warm or a compact activation request.

For a persisted v0.2.0 reset baseline with `WindowLength == 0`, a later
authoritative quota observation may fill the length. If that same observation
has explicit zero usage and its reset is within three minutes of
`observed_at + window_length`, mark it `SuspectedLazy` and schedule an immediate
precheck. Otherwise retain normal reset scheduling.

## Read-Only External-Reset Observations

When `enable_reset_probe` is true and a reset baseline has a known window
length, the Probe subsystem schedules a read-only quota observation at the
earlier of:

- reset maturity plus the configured post-reset delay; or
- `now + max(quota_refresh_interval, 30 minutes)`.

This observation belongs to Probe, not normal refresh. It may run while normal
refresh is dormant and reuses authoritative roster admission, credential
validation, coordinator concurrency, failure isolation, and all existing
background-request gates.

The schedule is rebuilt directly from durable known-length baselines after
restart. It does not require the in-memory quota cache and does not persist raw
quota snapshots.

An unchanged active window only receives its next read-only observation; it
does not send a compact request.

## Lazy External Reset And Compact Send

A read-only precheck is treated as a suspected external lazy reset only when:

1. the snapshot is valid;
2. usage is explicitly zero;
3. reset and authoritative window length are present;
4. `abs(reset_at - (observation_time + window_length)) <= 3 minutes`; and
5. the reset is newly observed relative to the previous baseline, or the
   baseline was a one-time v0.2.0 unknown-length migration.

Before classification, rebase the reset baseline to the new reset and set
`SuspectedLazy = true`. The existing suspected-lazy precheck rule then produces
`StillLazy`, allowing exactly one compact request through the current single-
lease/WAL/fence sequence. Verify continues through the existing generic
classifier so reset movement or positive usage can confirm activation.

Changing usage from zero to zero is not activation evidence. Usage-only
activation requires `old > 0 && new == 0`.

A genuine server reset can temporarily satisfy the same conservative signature
before any usage occurs. Because `enable_reset_probe` is explicitly opt-in, the
accepted bounded risk is one tiny verification request; single-flight, attempt
ownership, suppression, and verify-first recovery still prohibit duplicate
sends.

## Confirmed Re-Arming

`Confirmed` is terminal only for the completed attempt. A later authoritative
quota observation with a present window replaces it with a fresh reset baseline
and `WaitingReset` schedule.

- If the later observation has the strict lazy signature, it is scheduled for
  immediate precheck with `SuspectedLazy`.
- Otherwise it is scheduled for the earlier reset/observation deadline.
- Missing or invalid window evidence cannot re-arm it.

Re-arming is instance-scoped and persists through the existing atomic state
store. It must not replace or delete another account's Probe windows.

## Recovery And Concurrency Invariants

The port must preserve all local safety fixes:

- `PendingCheck.Deadline` remains zero;
- busy launches are coalesced and rerun before the Probe runner becomes idle;
- only one Probe sequence holds the per-instance coordinator lease;
- terminal windows and exact `AttemptID` deletion are one atomic write;
- a failed terminal write retains the original attempt ID and send fence;
- restart is verify-first and never emits a second activation POST;
- terminal completion merges only the completing instance;
- lifecycle logs are emitted only after their authoritative phase boundary and
  never contain credentials, headers, request/response bodies, or unredacted
  upstream errors.

## Management Disclosure

English and Chinese settings copy must explain that enabling automatic reset
activation also permits read-only background quota observations while ordinary
refresh is dormant. The interval is the configured quota refresh interval with
a 30-minute minimum. A compact request is sent only after the strict lazy-reset
signature is observed and may consume a small amount of quota.

No new setting is added; `enable_reset_probe` remains the single opt-in gate.

## Tests And Acceptance Gates

TDD coverage must include:

- fresh FiveHour, Weekly, and explicit-duration Monthly lazy windows;
- v0.2.0 unknown-length baseline migration and restart without quota cache;
- missing usage, missing duration, non-zero usage, and reset outside tolerance;
- observation deadline calculation and 30-minute floor;
- unchanged observation rescheduling without compact send;
- external reset detection while normal refresh is dormant;
- sliding-reset precheck sends once and verify converges;
- `Confirmed` re-arms from a later authoritative observation;
- genuine multi-account overlap preserves both instances and coalesces wakeups;
- terminal write failure/restart retains original attempt/fence and sends no
  duplicate POST;
- Management copy and lifecycle log redaction.

Before release, run:

- focused Probe tests;
- `go test ./... -count=1 -timeout 300s`;
- `go test -race ./... -count=1 -timeout 300s`;
- `go vet ./...`;
- `scripts/check_refactor_gates.ps1 -Stage S7` with 49 exact Mock row owners;
- Windows AMD64 DLL build and SHA-256;
- the repository's complete release build workflow or equivalent supported
  cross-platform build matrix.

## v0.2.1 Release And PR Closure

After every gate passes:

1. change runtime, Makefile, tests, README, and release metadata from `0.2.0`
   to `0.2.1` where they represent the current release;
2. add concise English and Chinese release documentation centered on lazy-window
   activation and observation behavior;
3. merge the reviewed feature branch into `main` without discarding the user's
   unrelated main-checkout changes;
4. verify the merged result, push `main`, create and push annotated tag
   `v0.2.1`, and confirm the GitHub release artifacts/checksums;
5. close PR #3 with a factual comment that v0.2.1 incorporated its baseline
   migration, re-arming, and dormant-observation ideas through the local safe
   Probe architecture, crediting the contributor without claiming a direct
   merge; and
6. update the Plugin Store entry to the published v0.2.1 artifact and verify the
   store displays the new version.

No GitHub PR, release, tag, or Plugin Store state is changed before local review
and all acceptance gates pass.
