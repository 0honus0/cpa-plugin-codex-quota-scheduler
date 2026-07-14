# Authoritative Roster Runtime Admission Design

## Status

Approved by the user on 2026-07-14.

This document records an S7 production-wiring correction. It does not edit the
frozen decision spec. `docs/deviations.md` records the clarification as
DEV-011.

## Problem

The roster controller can publish a healthy confirmed Capability-A roster, and
`PublishAuthoritativeRoster` successfully reconciles bindings, persists the
roster, and updates the immutable scheduler snapshot. It does not publish the
same highest-tier set into `PluginState` CPA admission.

Management and background refresh read the versioned runtime admission before
calling `ListAuths`, `GetAuth`, or OpenAI. With `Observed == false`, refresh
returns successfully without doing work. The UI therefore reports a healthy
roster while `codex_auth_count` remains zero and manual refresh is a no-op.

## Decision

Successful authoritative roster publication must also publish versioned
runtime admission:

```text
Observed: true
Priority: highest authoritative Codex priority
AuthIDs:  exactly the active highest-tier Codex IDs
```

This admission is derived only from the authoritative normalized roster. It is
never derived from scheduler candidates, cached accounts, Management state, or
partial binding results.

## Publication Order

`PublishAuthoritativeRoster` remains fail-closed. Runtime admission is replaced
only after all prerequisite work succeeds:

1. Normalize lifecycle and prove Capability-A.
2. Compute and filter the highest Codex tier.
3. Reconcile bindings and complete credential genesis.
4. Read back persisted bindings and validate instance identities.
5. Persist the confirmed roster.
6. Activate current instances and fence/cancel removed instances.
7. Recover/reconcile Probe state for the published bindings.
8. Replace `PluginState` CPA admission with the authoritative priority and IDs.
9. Publish the scheduler snapshot and runtime roster, update the credential
   adapter, and start the requested background controller.

No runtime refresh call is authorized before step 8. If any earlier step fails,
the previous admission remains in force and the new roster is not exposed. On
initial Capability-B startup, that means no admission and zero background
calls.

Steps 8 and the roster-view updates in step 9 execute under one publication
boundary. An admitted auth scan holds the matching read boundary while it
validates the admission version and copies roster entries. A concurrent refresh
therefore observes either the complete old admission/roster pair or the complete
new pair; it can never use a new admission with an old AuthIndex.

## Replacement and Fencing

`PluginState.ReplaceCPAAdmission` is the single version boundary. Any change to
priority or membership increments the admission version and prunes accounts no
longer admitted.

Therefore deletion, demotion, promotion to a newly higher tier, and same-tier
membership replacement all replace the complete admission set rather than
merging it. In-flight refresh work must re-check the admission version at each
existing permit boundary; results from an obsolete version cannot be applied
or written back.

An identical authoritative publication may reuse the current version. This is
safe because membership and priority are unchanged.

## Management and Background Flow

After successful publication, Management global refresh and ordinary initial
refresh use the same versioned admission:

```text
authoritative roster
  -> runtime admission
  -> admitted auth scan from the confirmed roster
  -> highest-tier filtering
  -> GetAuth
  -> OpenAI quota request
  -> fenced state publication
```

The admitted auth scan records `last_auth_scan`, `codex_auth_count`, and the
highest-tier accounts. The production runtime uses the already confirmed
roster instead of issuing another mutable host `ListAuths` call. Lower tiers
and non-Codex entries remain excluded.

## Error Handling and Safety

- Capability-B, malformed, empty, or no-Codex roster publication does not
  create admission.
- Failed binding genesis, persistence, cancellation, or Probe recovery does not
  expose the new admission.
- Lower-tier credentials never become admitted merely because `ListAuths`
  returns them.
- Admission publication must not trigger OpenAI calls by itself; it only makes
  subsequent authorized Management/background refresh calls effective.
- Existing per-call admission-version checks remain the stale-result fence.

## Tests

Production-wiring coverage must prove:

1. Publishing a confirmed authoritative roster populates runtime admission
   without any test-only `ReplaceCPAAdmission` call.
2. A subsequent Management/manual refresh calls `ListAuths`, `GetAuth`, and the
   quota HTTP endpoint for every admitted highest-tier Codex account.
3. Lower-tier and non-Codex auths returned by the host are ignored.
4. Deletion or demotion replaces admission and removes stale accounts.
5. A newly higher tier replaces the prior tier and increments the admission
   version.
6. A stale in-flight refresh cannot publish after admission replacement.
7. Failed roster publication remains fail-closed and makes zero new runtime
   calls.
8. A refresh paused across final publication cannot consume the new admission
   with the old roster or old AuthIndex.

Verification includes focused production-wiring tests, the S7 refactor gate,
full tests, race, vet, diff checking, a Windows DLL build, and browser acceptance
through the authenticated Management page.

## Non-Goals

- No candidate-derived admission.
- No partial or additive admission merge.
- No change to roster TTL, Degraded, FailClosed, or provisional-risk policy.
- No direct refresh as a side effect of roster publication.
- No authorization of lower-tier Codex or other-provider credentials.
