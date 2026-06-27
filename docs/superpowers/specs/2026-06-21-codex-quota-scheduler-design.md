# Codex Quota Scheduler Design

Date: 2026-06-21

## Goal

Build a CLIProxyAPI scheduler plugin for Codex accounts that behaves like an
optimized Fill First scheduler. The plugin should avoid burning one account to
exhaustion while other accounts with earlier quota expiry remain unused.

The v1 scheduler is intentionally rule-based, not score-based:

1. Respect CPA's configured auth priority first.
2. Inside the selected CPA priority tier, classify accounts as weekly-limited or
   monthly-limited.
3. Exclude accounts that are unavailable because a required quota window is
   exhausted.
4. Sort the remaining accounts by the relevant reset or expiry time according to
   the configured monthly mode.

The plugin must work without modifying CLIProxyAPI core. It should read Codex
quota information through host callbacks, keep quota state inside the plugin,
and use that state to classify, filter, and order scheduler candidates.

## Plugin Name And Workspace

The plugin folder and plugin ID are:

```text
codex-quota-scheduler
```

All plugin source, tests, build files, and docs live under this folder.

## CPA Capabilities Used

The plugin declares these CLIProxyAPI plugin capabilities:

- `scheduler`: makes the account pick for Codex requests.
- `usage_plugin`: receives completed request records and updates quota cache
  immediately on quota-like failures.
- `management_api`: exposes plugin status and diagnostic resources.

The plugin uses host callbacks instead of requiring the CPA Management API key:

- `host.auth.list`: discover runtime auth records and map `auth_id` to
  `auth_index`.
- `host.auth.get`: read the physical Codex auth JSON by `auth_index`.
- `host.http.do`: query the Codex quota endpoint using the account credentials.
- `host.log`: write redacted diagnostics.

The plugin does not rely on scheduler candidate `attributes` containing quota
fields. Current CPA candidates are safe copies and do not carry Codex quota
windows.

## Quota Source

The first implementation uses the same upstream quota source used by CPA
Management Center:

```text
GET https://chatgpt.com/backend-api/wham/usage
```

Required request data:

- `Authorization: Bearer <access_token>`
- `Chatgpt-Account-Id: <chatgpt_account_id>`
- Codex-like `User-Agent`

The plugin extracts:

- `access_token` from the Codex auth JSON.
- `id_token` from the auth JSON or nested metadata.
- `chatgpt_account_id` from `id_token`, including the nested
  `https://api.openai.com/auth.chatgpt_account_id` form.
- `plan_type` from auth attributes or the quota response.
- `subscription_active_until` from auth metadata or id token when available.

The plugin parses:

- `rate_limit.primary_window`
- `rate_limit.secondary_window`
- `code_review_rate_limit`
- `additional_rate_limits`
- `rate_limit_reset_credits`

Windows are classified by `limit_window_seconds`:

- 18,000 seconds: 5-hour limit.
- 604,800 seconds: weekly limit.
- 28-31 days: monthly limit.

If duration fields are missing, primary/secondary order is used as a fallback.

## In-Memory State

Quota state is keyed by `auth_id` and also indexed by `auth_index`.

Each account stores:

- `auth_id`
- `auth_index`
- display name or email
- provider
- CPA auth priority
- plan type
- account type: weekly, monthly, unknown
- quota windows: 5h, weekly, monthly, code-review, additional
- remaining percentage and used percentage
- reset or expiry time used by scheduler ordering
- rate-limit reset credits count
- last refresh time
- last successful refresh time
- last error, redacted
- stale flag
- temporary failure override from `usage_plugin`
- user annotation: alias, notes, tags, group ID

Tokens are never written to disk, never shown in the management resource, and
never logged. The plugin may keep tokens only long enough to make the refresh
request.

## Account Annotations

The plugin supports user-managed annotations so large account pools are easier
to operate.

Annotation features:

- `alias`: a short display name for one account.
- `notes`: longer free-form notes for one account.
- `tags`: zero or more labels for filtering and visual grouping.
- `group`: a user-defined group, useful for Team accounts, shared ownership, or
  operational pools.

Group metadata stores:

- `group_id`
- display name
- notes
- tags
- optional color

Annotations are separate from quota state. They do not affect the upstream
credential unless the user explicitly chooses a future auth-file sync feature.
This avoids rewriting sensitive Codex auth JSON for ordinary UI changes.

Default persistence:

- Store annotations in a plugin-owned JSON state file.
- The file path defaults to a local plugin state path and can be overridden by
  config.
- If the state file cannot be written, annotations still work in memory and the
  management resource shows a persistence warning.
- `annotations` in plugin config are used as initial seed data and as a
  read-only fallback when no state file exists. Once the state file exists, it
  wins over config annotations.

Stable annotation keys:

1. `auth_id`
2. `chatgpt_account_id`
3. email/name fallback

The plugin keeps a small alias map so annotations survive minor account index
changes when `auth_id` and ChatGPT account ID are available.

Annotation data model:

```json
{
  "accounts": {
    "<stable-account-key>": {
      "alias": "Team A Plus 01",
      "notes": "Shared Team A account; avoid heavy code review usage.",
      "tags": ["team-a", "plus"],
      "group_id": "team-a"
    }
  },
  "groups": {
    "team-a": {
      "name": "Team A",
      "notes": "Weekly pool used by backend work.",
      "tags": ["shared-team"],
      "color": "#4f46e5"
    }
  }
}
```

Scheduling behavior in v1 ignores annotation metadata. Groups, tags, aliases,
and notes are used only for display, filtering, and aggregate summaries.

## Refresh Model

The background refresher runs on:

- plugin registration
- plugin reconfiguration
- fixed interval, default 60 seconds
- manual refresh from plugin management resource
- optionally after quota-like failure events

Refreshes are concurrency-limited. Scheduler picks never perform upstream
network calls; they read the current cache only.

If quota refresh fails for one account, that account keeps its last good quota
state until `stale_after` expires. After that, the account is treated as stale
and is not selected by the plugin unless every candidate in the relevant CPA
priority tier lacks usable fresh quota data.

## Scheduler Behavior

The scheduler only handles Codex candidates when `handle_enabled` is true.
Non-Codex requests return `Handled=false` so CPA can continue with its normal
scheduling.

The plugin is intended to be enabled when the user wants optimized Fill First
behavior. Current CPA scheduler plugin requests do not expose the active CPA
built-in selector mode, so v1 cannot reliably auto-detect whether CPA is set to
Fill First or Round Robin. The explicit `handle_enabled` switch is the control
surface.

If the plugin is disabled or the request is not for Codex, it returns
`Handled=false`. If the plugin is enabled for a Codex request but cannot make a
confident pick from fresh quota state, it delegates to the configured built-in
fallback. The default and recommended fallback is CPA's built-in Fill First.

### CPA Priority First

CPA auth priority has higher precedence than all plugin ordering rules.

The plugin groups scheduler candidates by `SchedulerAuthCandidate.Priority`.
Higher numeric priority wins. It evaluates the highest priority tier first. It
only considers a lower priority tier when every candidate in the higher tier is
unavailable, stale without usable quota data, unknown, or otherwise not
selectable by the plugin.

This preserves the same high-level semantics as CPA's built-in schedulers:

```text
priority 10 candidates
  -> plugin ordering rules

priority 5 candidates
  -> considered only if priority 10 has no selectable account
```

### Account Availability

Weekly-limited accounts are available only when both required windows are
available:

```text
5h remaining > 0 && weekly remaining > 0
```

If either the 5-hour quota or weekly quota is exhausted, the account is
unavailable until the exhausted window resets.

Monthly-limited accounts are available when monthly quota remains:

```text
monthly remaining > 0
```

If a monthly account's monthly quota is exhausted, it is unavailable until the
monthly window resets.

Accounts with unknown family or missing quota data are not preferred by the
plugin. If all candidates in the active CPA priority tier are unknown or stale,
the plugin should delegate to the configured built-in fallback.

### Weekly Ordering

Within the active CPA priority tier, available weekly accounts are ordered by
their weekly reset or expiry time:

```text
earlier weekly reset/expiry first
```

The 5-hour window is an availability gate only. It does not decide the main
ordering among weekly accounts.

### Monthly Modes

The plugin supports two monthly scheduling modes.

#### `priority`

Monthly accounts are preferred inside the active CPA priority tier.

Ordering:

1. Available monthly accounts, sorted by monthly reset/expiry time.
2. Available weekly accounts, sorted by weekly reset/expiry time.

This mode is for users who intentionally want to consume monthly-limited
accounts before weekly-limited accounts at the same CPA priority.

#### `expiry_order`

Weekly and monthly accounts are merged into one ordered list inside the active
CPA priority tier.

Ordering:

```text
earlier long-window reset/expiry first
```

For weekly accounts, the long-window time is the weekly reset/expiry time. For
monthly accounts, the long-window time is the monthly reset/expiry time.

This mode naturally handles cases where a monthly account expires earlier than
weekly accounts and cases where a weekly account expires earlier than monthly
accounts. No separate special override is needed.

### Tie Breakers

When two selectable accounts have the same CPA priority, same account family
ordering position, and same reset/expiry time, ties are broken by:

1. CPA auth priority, already equal in the current tier.
2. provider-specific candidate readiness from CPA status.
3. stable `auth_id` ordering.

No remaining-usage percentage is used as a tie breaker in v1. Remaining usage is
only used to decide whether a quota window is available.

## Failure Feedback

The plugin declares `usage_plugin` and consumes `UsageRecord` after a request
finishes. A record includes `Provider`, `AuthID`, `AuthIndex`, `Model`, `Failed`,
`Failure.StatusCode`, and `Failure.Body`.

When `Provider == "codex"` and the failure looks quota-related, the plugin
updates its in-memory quota cache immediately instead of waiting for the next
poll.

Quota-like failure signals include:

- HTTP 429 with `error.type == "usage_limit_reached"`
- body containing `resets_at`
- body containing `resets_in_seconds`
- known Codex usage-limit messages

When a signal has reset timing:

- mark the relevant account or quota window exhausted
- set the reset time from the response when available
- force a refresh soon after the reset time

When a signal has no reset timing:

- mark a temporary quota failure
- apply a short exponential cooldown, capped by config
- schedule a quota refresh soon

Transient rate limits that are not usage exhaustion should not permanently mark
the account exhausted. The plugin follows CPA's distinction between Codex
`usage_limit_reached` and generic `rate_limit_error`.

## Visual Configuration And Management UI

The plugin should provide useful UI without requiring CPA Management Center code
changes.

### ConfigFields

The plugin registration metadata exposes `ConfigFields` so CPA Management Center
can render basic plugin configuration:

- `handle_enabled`
- `quota_refresh_interval`
- `stale_after`
- `monthly_mode`: `priority`, `expiry_order`
- `fallback`: `fill-first`
- `enable_usage_feedback`
- `annotation_state_path`

The plugin still validates config in `plugin.register` and
`plugin.reconfigure`; UI fields are not the source of truth.

### Management Resource

The plugin registers a resource such as:

```text
/v0/resource/plugins/codex-quota-scheduler/status
```

The resource displays the current scheduler priority order. Opening the page
should make it immediately clear which account would be selected if a Codex
request arrived at that moment.

The resource route is not a write-action API. It is exposed under
`/v0/resource/plugins/...`, which CPA does not protect with the Management key.
It must not mutate plugin state, import configuration, replace annotations,
trigger quota refresh, or call privileged host callbacks through query
parameters such as `action` or `payload`.

The primary account list is sorted by the plugin's effective scheduling order,
not by account name, CPA auth ID, or original config order.

The resource displays:

- ordered account list with the next selected account at the top
- CPA priority tier
- account alias, notes, tags, and group
- group list with aggregate quota status
- plan type
- account family: weekly, monthly, unknown
- 5h, weekly, monthly windows
- remaining/used percentage for display
- reset or expiry time used for ordering
- availability state and unavailable reason
- cache age and stale state
- monthly mode and effective config
- last selected account and selection reason
- last refresh error, redacted
- manual refresh action
- account annotation editor
- group editor
- tag filter and group filter

This is enough for day-to-day operation. A native quota page inside CPA
Management Center can be added later, but it is not required for v1.

### Management Routes

The plugin resource page can call plugin-owned management routes for controlled
actions after the user supplies the CPA Management key. The page may keep the key
only in memory for the current browser page session; it must not write the key to
local storage, session storage, plugin state, logs, exports, or rendered status
payloads.

- `GET /plugins/codex-quota-scheduler/status`: JSON status snapshot.
- `POST /plugins/codex-quota-scheduler/refresh`: trigger refresh for all or one
  account.
- `PUT /plugins/codex-quota-scheduler/settings`: update scheduler settings.
- `GET /plugins/codex-quota-scheduler/logs`: read scheduler logs.
- `GET /plugins/codex-quota-scheduler/export`: export scheduler settings and
  annotations.
- `POST /plugins/codex-quota-scheduler/import`: import scheduler settings and
  annotations.
- `GET /plugins/codex-quota-scheduler/annotations`: read aliases, notes, tags,
  and groups.
- `PUT /plugins/codex-quota-scheduler/annotations`: replace annotation state.
- `PATCH /plugins/codex-quota-scheduler/annotations/account`: update one
  account annotation.
- `PATCH /plugins/codex-quota-scheduler/annotations/group`: update one group.

These routes are plugin-owned CPA Management API routes, so CPA still protects
them with the management key. They do not expose raw tokens.

Security review must check that resource routes never reintroduce
`GET /v0/resource/plugins/codex-quota-scheduler/status?action=<action>` or any
equivalent query/body dispatch that mutates state or triggers credential-bearing
requests. Review must also check that `quota_endpoint` remains restricted to
`https://chatgpt.com/backend-api/wham/usage`, because quota refresh sends Codex
credentials as `Authorization: Bearer <access_token>` to that endpoint.

## Configuration

Minimal config:

```yaml
plugins:
  configs:
    codex-quota-scheduler:
      enabled: true
      handle_enabled: true
      quota_refresh_interval: 60s
      stale_after: 10m
      monthly_mode: expiry_order
      fallback: fill-first
      enable_usage_feedback: true
      annotation_state_path: ""
```

Advanced config:

```yaml
plugins:
  configs:
    codex-quota-scheduler:
      enabled: true
      handle_enabled: true
      quota_refresh_interval: 60s
      stale_after: 10m
      monthly_mode: priority
      fallback: fill-first
      enable_usage_feedback: true
      max_refresh_concurrency: 4
      annotation_state_path: "C:\\cpa-state\\codex-quota-scheduler.annotations.json"
      quota_endpoint: https://chatgpt.com/backend-api/wham/usage
      annotations:
        groups:
          team-a:
            name: Team A
            notes: Shared weekly pool
            tags: [team-a]
        accounts:
          account-key:
            alias: Team A Plus 01
            notes: Keep for backend work
            tags: [team-a, plus]
            group_id: team-a
```

## Error Handling

Refresh errors do not block scheduler picks.

Error categories:

- missing auth index: account cannot be queried; fallback ordering only
- missing access token: account cannot be queried; fallback ordering only
- missing ChatGPT account ID: account cannot be queried; fallback ordering only
- upstream unauthorized: mark refresh error, let CPA auth lifecycle handle auth
- upstream forbidden/payment issue: mark account unavailable until refreshed
- upstream quota response invalid: keep last good state, mark stale later
- network/proxy failure: keep last good state and retry with backoff

If all candidates in the active CPA priority tier have missing or stale quota
state, the plugin delegates to the configured built-in fallback rather than
guessing from incomplete data.

The management resource must redact tokens, cookies, full authorization headers,
and raw auth JSON.

## Testing Strategy

Unit tests:

- auth JSON parsing
- JWT/id token account ID extraction
- quota payload parsing
- window classification: 5h, weekly, monthly
- account family classification
- availability rules for weekly and monthly accounts
- CPA priority tier selection
- weekly ordering by weekly reset/expiry
- monthly `priority` mode
- monthly `expiry_order` mode
- stale cache behavior
- quota-like failure detection
- annotation key resolution
- annotation persistence read/write
- fallback scheduler delegation

Integration-style tests:

- fake host callbacks for auth list/get/http
- scheduler pick with mixed weekly/monthly accounts
- scheduler pick preserves CPA priority before plugin ordering
- usage feedback immediately changes next pick
- management resource returns redacted status
- management status list matches effective scheduler order
- management routes edit account alias, notes, tags, and groups

Manual verification:

- run plugin against local CPA with several Codex accounts
- compare displayed quota with CPA Management Center
- confirm UI top account matches the account selected by scheduler
- simulate 429 usage-limit failure and confirm the next pick avoids that account

## Open Questions

No blocking questions remain for v1. The first implementation uses host
callbacks. A Management API `/api-call` backend can be added later if users want
behavior identical to CPA Management Center or prefer CPA to perform token
substitution.
