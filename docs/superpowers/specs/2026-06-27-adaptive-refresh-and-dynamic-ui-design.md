# Adaptive Refresh And Dynamic UI Design

## Context

The plugin currently keeps quota state by actively calling ChatGPT quota
endpoints for every eligible Codex account. The default interval is 30 minutes,
and `plugin.register` / `plugin.reconfigure` starts the refresher and schedules
an immediate refresh. This creates unnecessary upstream traffic when the user is
idle for many hours.

The management UI also calls `window.location.reload()` after settings,
refresh, import, and annotation actions. Because the CPA Management key is held
only in the page input, a full reload discards it and forces the user to enter
it again.

CPA plugin-store review also requires a strict boundary:

- `/v0/resource/plugins/...` serves resource content only.
- State-changing and privileged operations must stay behind CPA Management API
  routes.
- Quota refresh must not allow `quota_endpoint` to point at arbitrary hosts.

`cpa-plugin-codex-invite` is the reference UI pattern: a resource page renders
the browser UI, while all privileged actions use `fetch` against
`/v0/management/...` with the Management key.

## Goals

- Reduce automatic upstream quota checks while preserving usable account
  scheduling.
- Keep 429 feedback as the fast path for stopping an exhausted account.
- Do not implement passive response-header quota learning in this version.
- Replace fixed background polling with an active-window due/retry queue.
- Make dynamic UI actions update page content without a full browser reload.
- Keep resource routes read-only and keep all writes behind Management API.
- Provide install-ready defaults so normal users do not need to tune settings.

## Non-Goals

- Do not parse successful Codex response headers for quota state.
- Do not synchronize every Codex request with a full quota refresh.
- Do not restore `GET /v0/resource/plugins/.../status?action=...`.
- Do not expose or persist the CPA Management key in browser storage.
- Do not make `quota_endpoint` user-changeable to non-ChatGPT hosts.

## Configuration

Add these settings:

- `refresh_active_window`: duration. Default `1h`.
- `refresh_after_reset_delay`: duration. Default `1m`.
- `refresh_retry_delays`: comma-separated duration string. Default
  `1m,5m,15m`.
- `refresh_on_startup`: boolean. Default `false`.

Keep or change these defaults:

- `stale_after`: `5h`
- `monthly_mode`: `expiry_order`
- `max_refresh_concurrency`: `1`
- `circuit_failure_threshold`: `5`
- `circuit_open_duration`: `30m`
- `circuit_half_open_success_threshold`: `2`
- `max_log_entries`: `200`
- `log_retention`: `24h`

`quota_refresh_interval` should stop driving a fixed full-account polling loop.
Retain it for backwards-compatible config decoding if needed, but remove it from
the normal UI and do not use it to refresh all accounts on a timer. Due times,
retry times, and the active window decide when upstream refreshes happen.

## Refresh Model

The refresher maintains two concepts:

- `last_codex_activity_at`: updated when `scheduler.pick` handles or observes a
  Codex request.
- per-account refresh metadata: last success, last failure classification,
  retry attempt, next retry time, and auth failure state.

The refresher is active only when:

- the current time is before `last_codex_activity_at + refresh_active_window`,
  or
- the user explicitly triggers manual refresh from the Management UI.

Plugin startup does not refresh by default. If `refresh_on_startup` is false,
`plugin.register` and `plugin.reconfigure` start the worker loop but do not
enqueue an immediate full refresh. The first Codex request can fall back using
current cached state while the background worker refreshes due accounts.

An account is due for refresh when at least one condition is true:

- it has never had a successful quota refresh;
- `last_success_at + stale_after` is in the past;
- a known 5-hour, weekly, or monthly reset time plus
  `refresh_after_reset_delay` is in the past;
- a 429-driven temporary pause or circuit probe time has elapsed;
- a transient refresh failure has a `next_retry_at` in the past.

The worker should refresh only due accounts. When scheduler candidates are
available, prefer due accounts from the current Codex candidate set and active
CPA priority tier before lower-priority accounts. Manual all-account refresh may
still refresh every eligible account because it is a user action.

## Failure Classification

Refresh failures should be classified before deciding whether to retry:

- `401`: authentication failure. Mark the account as requiring re-login, display
  a clear message, and do not automatically retry quota refresh for that account.
- local credential extraction failures, such as malformed auth JSON or missing
  token fields: non-retryable local credential errors. Display the concrete
  sanitized reason, but do not classify these as upstream `401` re-login events.
- `403`: transient upstream/environment failure, not authentication expiry.
- timeout, connection error, host HTTP request failure, `5xx`, quota endpoint
  `429`, and temporary parse failures: transient failure. Keep the account in
  the retry queue.

Transient retry delays default to:

1. `1m`
2. `5m`
3. `15m`

After the final configured delay, continue retrying at the final delay while
the plugin is inside the active window. Once the active window expires, retries
pause until the next Codex request or manual refresh.

## Usage Feedback

Keep the existing 429-oriented usage feedback model:

- If a Codex request returns `usage_limit_reached`, mark the selected account
  unavailable until the parsed reset time.
- If no reset time is available, keep the current short temporary pause behavior
  by marking the account unavailable for 2 minutes, then make it due for refresh
  inside the active window.
- Successful requests do not need to parse response headers in this version.

This preserves the simple operational rule: if an account works, continue using
it; if it hits quota, stop using it and move to the next account.

## Management UI

Use the Codex Invite resource-page pattern:

- `/v0/resource/plugins/codex-quota-scheduler/status` renders the UI shell.
- All privileged operations continue to use
  `/v0/management/plugins/codex-quota-scheduler/...`.
- The page keeps the Management key only in the password input for the current
  browser session.
- No action calls `window.location.reload()`.

Move the main "Refresh quota" action outside the settings panel.

Render scheduler settings as a collapsed `<details>` panel by default. The
summary should say that defaults are already configured and normal users do not
need to adjust them. The collapsed panel contains:

- scheduler takeover toggle;
- monthly mode;
- stale/cache and adaptive refresh settings;
- refresh concurrency;
- circuit breaker settings;
- log settings;
- Save settings;
- Export config;
- Import config.

After actions complete:

- Save settings: fetch fresh status JSON and re-render settings, metrics,
  account cards, and logs.
- Refresh quota: show a background-refresh notice, then poll status/logs a few
  times without reloading.
- Refresh one account: update that account card and logs without reloading.
- Save account annotations: update the account card, group data, and logs
  without reloading.
- Import config: fetch fresh status JSON and re-render without reloading.
- Refresh logs and export logs: update or download without page reload.

The page should continue to work if localStorage is unavailable. It may store
non-secret UI preferences such as locale, but it must not store the CPA
Management key, Codex tokens, cookies, or proxy settings that the user expects
to be session-only.

## Security Boundary

The resource route must never perform writes, import state, change settings,
change annotations, trigger refresh, call host auth callbacks, or call upstream
quota endpoints.

Allowed resource behavior:

- `GET /v0/resource/plugins/codex-quota-scheduler/status` returns HTML/CSS/JS UI
  content.

Allowed state-changing behavior:

- only registered `/v0/management/plugins/codex-quota-scheduler/...` routes,
  protected by the CPA Management key.

`quota_endpoint` validation remains strict: accept only
`https://chatgpt.com/backend-api/wham/usage`. Import and settings updates must
not weaken this restriction.

## Status And Visibility

Account cards should show enough state to make adaptive refresh understandable:

- cache age;
- last successful refresh;
- last refresh error;
- auth failure / re-login required state;
- next retry time for transient failures;
- due reason when available;
- circuit state and next probe time.

Logs should be capped by the new default `max_log_entries: 200`. Important
events include active-window activation, due refresh scheduling, refresh
success, transient retry scheduling, auth failure, manual refresh, and security
validation errors.

## Testing

Unit tests should cover:

- default config values;
- config decoding and normalization for the new adaptive refresh settings;
- startup does not enqueue immediate refresh when `refresh_on_startup` is false;
- Codex scheduler activity updates the active window;
- idle active-window expiry prevents automatic due/retry refresh;
- due account selection from stale, reset, 429 pause, and retry states;
- transient retry delay progression;
- `401` auth failure stops automatic retry;
- `403` is treated as transient retryable failure;
- `quota_endpoint` cannot be changed to non-ChatGPT hosts;
- resource route rejects non-GET writes and action-style query mutations;
- UI template renders settings as a collapsed panel;
- UI script no longer contains `window.location.reload()`.

Integration-style tests should cover:

- management save/import/annotation/refresh routes still require Management API
  routing;
- status JSON can support dynamic re-rendering after mutations;
- manual refresh still works outside the active window.

## Acceptance Criteria

- With no Codex requests for more than `refresh_active_window`, the plugin does
  not automatically call upstream quota endpoints.
- The first Codex request after idle can use fallback/cached scheduling and
  starts background due refresh without blocking the request.
- Due refreshes are account-scoped and retry transient failures using
  `1m -> 5m -> 15m`.
- `401` displays re-login required and stops retrying that account
  automatically.
- `403` is retryable and does not display re-login required.
- The UI no longer loses the Management key after ordinary actions because no
  action hard-refreshes the page.
- The settings panel is collapsed by default and describes the install-ready
  defaults.
- Resource routes remain read-only UI content, and all state-changing operations
  remain behind CPA Management API routes.
- `quota_endpoint` remains restricted to the expected ChatGPT usage endpoint.
