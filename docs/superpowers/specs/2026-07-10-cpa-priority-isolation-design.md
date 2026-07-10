# CPA Priority Isolation and Internal Scheduling Design

## Status

Approved on 2026-07-10.

## Problem

CLIProxyAPI (CPA) currently passes scheduler candidates grouped by CPA auth
priority. When every account in the highest CPA priority tier is unavailable,
delegating to CPA's built-in `fill-first` scheduler does not fall through to
usable accounts in lower CPA priority tiers. This can leave requests without a
usable account even though lower-tier capacity remains.

The plugin must not attempt to repair CPA itself. Until CPA provides and the
plugin verifies an upstream fix, the plugin will isolate itself from CPA
priority semantics and maintain its own per-account scheduling priority.

## Goals

- Report the priority-tier fallback defect in the official CPA repository.
- Track the plugin-side mitigation in a remote issue in this repository and
  link it to the upstream CPA issue.
- Admit only Codex accounts belonging to the highest CPA priority present in
  the current CPA candidate set.
- Exclude every lower CPA priority account from plugin quota refresh, status,
  management, and scheduling state.
- Maintain a plugin-owned account priority that defaults to `0`, is editable,
  persists with annotations, and never reads from or writes to CPA priority.
- Fall through from an exhausted plugin priority tier to the next plugin
  priority tier before considering built-in fallback.
- Close the plugin issue after the mitigation is released, leaving a note that
  CPA priority integration may be reconsidered after the upstream fix is
  released and verified.

## Non-goals

- Modifying or patching the CPA repository.
- Writing plugin priority values into CPA auth files or CPA management APIs.
- Loading lower CPA priority accounts for background quota refresh or status
  display.
- Automatically restoring CPA priority integration merely because the upstream
  issue is closed.
- Changing quota parsing, reset probing, circuit breaking, or the existing
  within-tier monthly/weekly ordering rules except where required to apply the
  new admission and priority boundaries.

## Architecture

The mitigation has two independent scheduling boundaries.

### 1. CPA admission boundary

For each scheduler candidate set, the plugin determines the maximum CPA
priority among Codex candidates. Only candidates with that exact priority are
admitted. Candidates below that value are rejected before quota refresh,
state presentation, or account selection.

The CPA priority is retained only as admission metadata and diagnostics. It is
not copied into the plugin-owned scheduling priority.

When all admitted CPA candidates have the same priority, all are admitted. The
recommended CPA configuration remains priority `0` for every account so the
plugin can manage the complete account pool.

Because CPA auth-file discovery may not contain candidate priority metadata,
the plugin must derive the admitted auth ID set from scheduler requests and
use that set to constrain later discovery and refresh operations. An account
that has not appeared in the current highest-priority admitted set must not be
introduced into the active plugin pool solely through an auth-file scan.

If the observed CPA maximum changes, the admitted set is replaced by the
accounts in the new maximum tier. Previously admitted lower-tier accounts are
removed from active scheduling and refresh state rather than remaining as
stale selectable entries.

### 2. Plugin scheduling boundary

`AccountAnnotation` gains a plugin-owned integer priority field named
`scheduler_priority`.

- Missing values behave as `0` for backward compatibility.
- Larger values have higher scheduling priority.
- The value is editable from the account management dialog.
- The value is included in annotation import, export, persistence, and status
  output.
- The field is never initialized from CPA candidate priority.
- Saving the field writes only the plugin annotation state.

The scheduler orders admitted accounts by `scheduler_priority` descending. It
checks every account in the highest plugin tier using the existing availability
and within-tier ordering rules. If that tier has no available account, it
continues to the next plugin tier. Built-in fallback is considered only after
all admitted plugin tiers have been checked.

## Data flow

1. CPA calls `scheduler.pick` with Codex candidates and CPA priorities.
2. The plugin calculates the maximum CPA priority in that request.
3. The plugin records the auth IDs in that maximum tier as the admitted set.
4. Lower CPA priority candidates are excluded from ordering and selection.
5. Refresh and discovery paths consult the admitted set before loading or
   refreshing accounts.
6. Admitted accounts receive `scheduler_priority` from plugin annotations,
   defaulting to `0`.
7. The scheduler evaluates plugin priority tiers from highest to lowest.
8. The first available account is selected according to the existing
   within-tier quota rules.
9. If no admitted account is usable, the configured fallback behavior runs.

## Management behavior

The account status model exposes both values with distinct names:

- `cpa_priority`: read-only admission metadata.
- `scheduler_priority`: editable plugin scheduling priority.

The management UI labels them explicitly as CPA priority and plugin priority.
The account editor accepts an integer plugin priority and saves it through the
existing annotation endpoint. Existing annotation documents without the field
remain valid.

The UI and public status payload show only admitted accounts. Lower CPA
priority accounts are not displayed as inactive cards because the requirement
is to stop loading them, not merely to mark them unavailable.

## State and lifecycle rules

- Before the first scheduler request supplies candidate priorities, the plugin
  may have no admitted account set. It must not perform an unconstrained
  full-account refresh that would load every CPA priority tier.
- A scheduler request refreshes the admitted set even when no account is
  selected.
- Manual refresh-all operates only on admitted accounts.
- Manual refresh-one rejects an auth ID outside the admitted set.
- Reconfiguration and restart preserve annotations but rebuild CPA admission
  state from new scheduler candidate observations.
- Removing an account from the admitted set removes it from active quota state;
  its annotation may remain on disk so a future re-admission restores the
  plugin priority and descriptive metadata.

## Error handling and diagnostics

- If a request contains no Codex candidates, existing no-candidate behavior is
  retained.
- If admitted candidates are unknown to quota state, they remain eligible for
  constrained discovery and refresh but are unavailable until valid quota data
  is obtained.
- Logs record the observed maximum CPA priority, admitted account count, and
  excluded lower-tier count without exposing credentials.
- Selection logs report `scheduler_priority` separately from
  `cpa_priority`.
- Rejected manual refresh requests return a clear error indicating that the
  account is outside the active CPA priority tier.

## Remote issue workflow

### CPA issue

Create an issue in `router-for-me/CLIProxyAPI` containing:

- A six-account reproduction: two accounts at CPA priority `1`, four at CPA
  priority `0`.
- Both priority `1` accounts have exhausted five-hour quota; all priority `0`
  accounts remain usable.
- The scheduler plugin delegates to `fill-first` with only the two exhausted
  accounts represented in the fallback outcome.
- Expected behavior: built-in scheduling falls through to usable lower CPA
  priority tiers when the highest tier has no usable account.
- Actual fallback reason and unavailable summary, with personal account IDs
  redacted.

### Plugin issue

Create an issue in
`JefferyZhang2019/cpa-plugin-codex-quota-scheduler` that:

- Links the CPA issue.
- Documents the temporary CPA admission boundary.
- Documents the new plugin-owned priority and tier fallback behavior.
- Defines release and verification acceptance criteria.

After the mitigation release is published and verified, close the plugin issue
with a note that CPA priority integration can be reconsidered only after the
upstream fix is available and independently verified.

## Versioning and compatibility

The current local repository contains unpushed commits tagged `v0.1.5`. They
must be preserved and reviewed before release work proceeds. The mitigation is
targeted for `v0.1.6` unless repository build metadata reveals a conflicting
version requirement.

Existing configuration and annotation files remain compatible. The new
annotation field defaults to zero when absent. The operational behavior change
is intentional: deployments that assign different CPA priorities will expose
only the maximum CPA priority tier to the plugin. Users who want every Codex
account managed by the plugin must configure all CPA account priorities
identically; priority `0` is recommended.

## Testing strategy

All production changes follow test-driven development.

- Admission tests prove only maximum-CPA-priority candidates enter ordering.
- Refresh tests prove lower CPA priority accounts are neither discovered nor
  refreshed.
- State tests prove a changed maximum tier replaces the admitted account set
  and removes excluded accounts from active state.
- Scheduler tests prove plugin priority defaults to zero, higher plugin tiers
  win, and exhausted plugin tiers fall through to lower plugin tiers.
- Annotation tests prove `scheduler_priority` persists and old files remain
  compatible.
- Management tests prove the field is displayed, edited, imported, exported,
  and never sent to CPA.
- Integration tests reproduce the six-account incident and verify selection of
  a usable account when all admitted accounts share one CPA priority.
- Regression tests verify built-in fallback runs only after every plugin
  priority tier is unavailable.
- Full `go test ./...`, formatting, static analysis, and release build checks
  must pass before publishing.

## Acceptance criteria

- Two CPA priority tiers can never coexist in the active plugin account pool.
- All accounts in the maximum observed CPA priority tier are admitted.
- No lower CPA priority account is refreshed, displayed, or scheduled.
- Plugin priority is independently editable and persistent, defaulting to zero.
- Plugin priority never changes a CPA auth file or CPA setting.
- Exhausting all accounts in one plugin priority tier causes selection to
  continue in the next plugin priority tier.
- The CPA and plugin remote issues exist and link to each other.
- The mitigation release is published with upgrade guidance recommending equal
  CPA priorities, preferably zero.
- The plugin issue is closed after release verification with the upstream
  reintegration note.
