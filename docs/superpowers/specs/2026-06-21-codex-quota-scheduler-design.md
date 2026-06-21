# Codex Quota Scheduler Design

Date: 2026-06-21

## Goal

Build a CLIProxyAPI scheduler plugin for Codex accounts that keeps Fill First's
simple preference for deterministic account choice, but avoids burning one
account to exhaustion while other accounts remain unused.

The plugin must work without modifying CLIProxyAPI core. It should read Codex
quota information through host callbacks, keep quota state in plugin memory, and
use that state to score scheduler candidates.

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
- plan type
- account type: weekly, monthly, unknown
- quota windows: 5h, weekly, monthly, code-review, additional
- remaining percentage and used percentage
- reset time
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
- optional scheduling hint

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

Scheduling behavior in v1 remains account-scored. Groups are used for display,
filtering, and aggregate summaries. The score engine reads group metadata only
for optional future policy hooks, such as group-level drain balancing or
group-level backup preference.

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
and receives a conservative score.

## Scheduler Behavior

The scheduler only handles Codex candidates. Non-Codex requests return
`Handled=false` so CPA can continue with its normal scheduling.

Candidate filtering:

- Ignore disabled or non-active candidates already filtered out by CPA if they
  still appear.
- Ignore candidates with internal quota state marked exhausted and reset time in
  the future, unless every candidate is exhausted.
- Prefer candidates with fresh quota data.
- Fall back to the configured builtin scheduler when quota data is unavailable.

Default account family policy:

- Weekly-limit accounts are primary.
- Monthly-limit accounts are backup.
- If a monthly account expires or resets earlier than all weekly accounts'
  weekly reset times, monthly accounts become primary until they are no longer
  urgent or are exhausted.

The initial scoring formula is intentionally simple and configurable:

```text
score =
  family_score
  + five_hour_remaining_weight * five_hour_remaining
  + long_window_remaining_weight * long_window_remaining
  + reset_urgency_weight * reset_urgency
  + freshness_weight * freshness
  + priority_weight * normalized_cpa_priority
  - stale_penalty
  - recent_failure_penalty
```

Where:

- `five_hour_remaining` is 0-1.
- `long_window_remaining` is weekly or monthly remaining, 0-1.
- `reset_urgency` increases when a long window reset or subscription expiry is
  near and there is still quota to consume.
- `freshness` decreases as quota cache ages.
- account family policy can override the final candidate pool before scoring.

Ties are broken by:

1. higher score
2. lower current usage for the account family
3. CPA priority
4. stable round-robin cursor
5. stable auth ID ordering

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

- mark the relevant account exhausted
- set the account or window reset time from the response
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

- `quota_refresh_interval`
- `stale_after`
- `monthly_policy`: `backup`, `prefer_when_expiring`, `prefer`
- `fallback`: `fill-first`, `round-robin`
- `enable_usage_feedback`
- `annotation_state_path`
- optional scoring weights

The plugin still validates config in `plugin.register` and
`plugin.reconfigure`; UI fields are not the source of truth.

### Management Resource

The plugin registers a resource such as:

```text
/v0/resource/plugins/codex-quota-scheduler/status
```

The resource displays:

- account list
- account alias, notes, tags, and group
- group list with aggregate quota status
- plan type
- account family: weekly, monthly, unknown
- 5h, weekly, monthly windows
- remaining/used percentage
- reset time
- cache age and stale state
- current score and score components
- last selected account and selection reason
- last refresh error, redacted
- manual refresh action
- account annotation editor
- group editor
- tag filter and group filter
- effective config

This is enough for day-to-day operation. A native quota page inside CPA
Management Center can be added later, but it is not required for v1.

### Management Routes

The plugin resource page can call plugin-owned management routes for controlled
actions:

- `GET /plugins/codex-quota-scheduler/status`: JSON status snapshot.
- `POST /plugins/codex-quota-scheduler/refresh`: trigger refresh for all or one
  account.
- `GET /plugins/codex-quota-scheduler/annotations`: read aliases, notes, tags,
  and groups.
- `PUT /plugins/codex-quota-scheduler/annotations`: replace annotation state.
- `PATCH /plugins/codex-quota-scheduler/annotations/account`: update one
  account annotation.
- `PATCH /plugins/codex-quota-scheduler/annotations/group`: update one group.

These routes are plugin-owned CPA Management API routes, so CPA still protects
them with the management key. They do not expose raw tokens.

## Configuration

Minimal config:

```yaml
plugins:
  configs:
    codex-quota-scheduler:
      enabled: true
      quota_refresh_interval: 60s
      stale_after: 10m
      monthly_policy: backup
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
      quota_refresh_interval: 60s
      stale_after: 10m
      monthly_policy: prefer_when_expiring
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
      weights:
        five_hour_remaining: 45
        long_window_remaining: 35
        reset_urgency: 15
        freshness: 5
        priority: 2
        stale_penalty: 30
        recent_failure_penalty: 40
```

## Error Handling

Refresh errors do not block scheduler picks.

Error categories:

- missing auth index: account cannot be queried; fallback score only
- missing access token: account cannot be queried; fallback score only
- missing ChatGPT account ID: account cannot be queried; fallback score only
- upstream unauthorized: mark refresh error, let CPA auth lifecycle handle auth
- upstream forbidden/payment issue: lower score and show diagnostic
- upstream quota response invalid: keep last good state, mark stale later
- network/proxy failure: keep last good state and retry with backoff

The management resource must redact tokens, cookies, full authorization headers,
and raw auth JSON.

## Testing Strategy

Unit tests:

- auth JSON parsing
- JWT/id token account ID extraction
- quota payload parsing
- window classification: 5h, weekly, monthly
- scoring
- monthly policy
- stale cache behavior
- quota-like failure detection
- annotation key resolution
- annotation persistence read/write
- fallback scheduler delegation

Integration-style tests:

- fake host callbacks for auth list/get/http
- scheduler pick with mixed weekly/monthly accounts
- usage feedback immediately changes next pick
- management resource returns redacted status
- management routes edit account alias, notes, tags, and groups

Manual verification:

- run plugin against local CPA with several Codex accounts
- compare displayed quota with CPA Management Center
- simulate 429 usage-limit failure and confirm the next pick avoids that account

## Open Questions

No blocking questions remain for v1. The first implementation uses host
callbacks. A Management API `/api-call` backend can be added later if users want
behavior identical to CPA Management Center or prefer CPA to perform token
substitution.
