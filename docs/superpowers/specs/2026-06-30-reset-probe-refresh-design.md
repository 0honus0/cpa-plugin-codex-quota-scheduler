# Reset Probe Refresh Design

## Context

Codex quota reset times returned by the ChatGPT usage endpoint can be lazy after
a window expires. If no real Codex request has started the next window, a quota
refresh can return a reset time close to `now + window_length`. A later refresh
then returns a reset time shifted later by the same elapsed time. The window is
not truly counting down.

The plugin already keeps an internal account quota cache in
`PluginState.accounts`. The scheduler reads this cache when ranking accounts,
and `QuotaRefresher` updates it through `host.http.do` calls to
`https://chatgpt.com/backend-api/wham/usage`. This feature should extend that
cache-driven flow instead of polling OpenAI more often.

CPA exposes `host.model.execute`, but the current callback request structure
does not provide a direct per-auth selector. Although a nested model execution
would likely re-enter scheduler selection, using that loop would require
short-lived in-memory target state and recursion guards. The first version will
use `host.http.do`, matching the existing quota refresh path, so each account can
be probed deterministically.

## Goals

- Add an opt-in setting for automatic reset probe refresh.
- Keep the setting disabled by default.
- Show a visible, non-collapsed warning in the management UI when presenting the
  setting.
- Use cached reset times to decide when to wake and confirm; do not poll all
  accounts continuously.
- After a cached reset matures, refresh quota once and detect lazy reset times by
  comparing the returned reset with `now + limit_window_seconds`.
- Send a minimal Codex compact probe only when the returned reset still appears
  lazy.
- Probe each eligible account independently.
- Refresh quota again after a successful probe so the cache stores the real
  counting window.
- Avoid repeated probes for the same account/window/reset cycle.
- Avoid guessing monthly reset arithmetic.

## Non-Goals

- Do not implement the `host.model.execute` scheduler-loop probe path in this
  version.
- Do not expose probe delay, probe threshold, endpoint, model, or payload as user
  settings.
- Do not let users configure arbitrary probe endpoints.
- Do not send probes unless the user explicitly enables the feature.
- Do not infer monthly reset duration as 30 days or calendar-month plus one.

## Configuration

Add one user-facing setting:

- `enable_reset_probe`: boolean, default `false`.

Use internal constants:

- `resetProbeAfterResetDelay = 10 * time.Minute`
- `resetProbeCloseThreshold = 3 * time.Minute`
- `codexResetProbeEndpoint = "https://chatgpt.com/backend-api/codex/responses/compact"`
- `codexResetProbeModel = "gpt-5.4-mini"`

The 10-minute delay is intentionally greater than the 3-minute close threshold.
If a window has really started and the plugin checks 10 minutes after reset, the
new reset time should be about `now + window_length - 10m`, which is outside the
3-minute lazy-reset threshold. If the window has not started, the returned reset
time should remain close to `now + window_length`.

There is one boundary case: if some other real Codex request starts the window
within roughly 3 minutes before the probe check, the returned reset can still be
close to `now + window_length`. The feature may send one extra tiny probe in
that case. The important guarantee is no tight loop and no repeated probe for
the same account/window/reset cycle after the state is verified or exhausted.

The existing `refresh_after_reset_delay` keeps its current meaning for normal
quota refresh. It must not be reused for probe classification.

## State Model

Extend `AccountState` with per-window probe state, keyed by `WindowKind`. A
single account can have both `five_hour` and `weekly`/`monthly` resets mature at
the same time, so one account-level probe state is not enough.

- window kind (`five_hour`, `weekly`, or `monthly`)
- window seconds captured when the pending probe is created
- the original cached reset time that matured
- next probe check time, initially `original_reset_at + 10m`, later a retry time
- last probe attempt time
- verified time
- attempt count
- status (`pending`, `confirmed_active`, `verified`, or `failed`)
- sanitized error text

The original reset time matters. A normal quota refresh at
`reset_at + refresh_after_reset_delay` may receive a lazy reset and overwrite the
visible quota reset. The plugin must still remember that the old reset matured
and should be checked at `old_reset_at + 10m`.

For monthly windows, the pending state may only be created when the old cached
window included a positive `limit_window_seconds`. Once created, later lazy-reset
classification uses the stored window seconds instead of re-deriving duration
from the refreshed quota response.

## Detection Flow

When an account is refreshed:

1. Load the existing cached account before replacing quota.
2. Request `/backend-api/wham/usage` through `host.http.do`.
3. Parse quota as today.
4. If reset probe is disabled, store quota normally.
5. If reset probe is enabled, inspect the previous cached windows.
6. For each previous window whose reset time has passed, create or preserve a
   pending probe check for `previous_reset_at + 10m`.
7. If no pending check is due yet, store quota and wake the refresh loop at the
   earliest pending check time.
8. For each due pending window, compare the newly returned matching window with
   `now + stored_window_duration`.
9. If the absolute difference is greater than 3 minutes, mark the probe as
   `confirmed_active` and do not send a probe.
10. If one or more due windows are within 3 minutes of `now + stored_duration`,
    send at most one compact probe for that account. One real Codex request
    should start all lazy windows for the account.
11. A successful compact probe requires positive usage evidence. Once usage is
    present, mark the affected lazy windows `verified` for this reset cycle and
    do not send another probe for those windows.
12. After a verified probe, refresh `/wham/usage` again and store the
    post-probe quota when that refresh succeeds. If that follow-up refresh
    fails, log/store only a sanitized warning and keep the probe verified; a
    second compact probe would spend extra tokens even though the first probe
    already started the window.
13. If the probe request fails or usage evidence is missing, keep the affected
    window pending and schedule a backoff retry. After the retry budget is
    exhausted, mark it `failed`. Do not retry in a tight loop.

Window duration is resolved this way:

- Prefer `QuotaWindow.LimitWindowSeconds`.
- Fallback to 5 hours for `WindowFiveHour`.
- Fallback to 7 days for `WindowWeekly`.
- Do not fallback for monthly windows when creating pending state. Monthly
  probing requires server-provided `limit_window_seconds`.
- Detection uses `ResetProbeState.WindowSeconds`, not the refreshed window's
  current `limit_window_seconds`, so monthly windows stay conservative.

## Probe Request

Use `host.http.do` with the account's current Codex credentials:

- Method: `POST`
- URL: `https://chatgpt.com/backend-api/codex/responses/compact`
- Headers:
  - `Authorization: Bearer <access_token>`
  - `Chatgpt-Account-Id: <chatgpt_account_id>`
  - `Accept: application/json`
  - `Content-Type: application/json`
  - `User-Agent: <existing quotaUserAgent>`

Request body:

```json
{"model":"gpt-5.4-mini","instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}]}
```

A 2xx response is not enough. Success requires at least one positive usage token
field in any of these paths:

- `usage.total_tokens`
- `usage.prompt_tokens`
- `usage.input_tokens`
- `usage.completion_tokens`
- `usage.output_tokens`
- `response.usage.total_tokens`
- `response.usage.prompt_tokens`
- `response.usage.input_tokens`
- `response.usage.completion_tokens`
- `response.usage.output_tokens`

## Scheduling

`PluginState.NextRefreshDueAt` should include pending reset probe checks when
`enable_reset_probe` is true. `accountRefreshDue` should return
`reset_probe_check_due` when a pending probe check is due.

This preserves the current cache-first behavior:

- no cached reset due means no probe confirmation request;
- cached reset due but probe delay not reached means schedule a later check;
- probe check due means one quota refresh, then maybe one compact probe.

The existing active-window behavior should continue to apply. The feature should
not wake a completely idle plugin forever unless current refresh-loop rules
already consider it active.

## Management UI

Add a checkbox in the scheduling settings:

- label: "Enable automatic reset probe"
- default unchecked
- maps to `enable_reset_probe`

Add a visible warning outside the collapsed settings panel:

> Automatic reset probe is off by default. Enable it only if you want the plugin
> to send a tiny Codex request after a reset appears lazy, so the next quota
> window starts counting down.

The warning must not be hidden inside a collapsed `<details>` element.

Account cards or status JSON should expose probe status, next check time, last
probe time, verified time, and sanitized probe error when present.

## Security And Privacy

- Keep endpoint validation strict; the probe endpoint is a fixed internal
  constant.
- Never log access tokens, ID tokens, refresh tokens, cookies, authorization
  headers, raw credential JSON, or full upstream response bodies.
- Store only sanitized probe errors.
- Sanitize probe errors with the existing credential-aware redaction path so
  access tokens, ID tokens, bearer headers, cookies, and raw credential values
  cannot leak through host errors or response summaries.
- Keep probe request bodies fixed and tiny.
- Do not expose the CPA Management key or Codex credentials in resource HTML or
  JSON.
- Do not add any state-changing or privileged operation under
  `/v0/resource/plugins/codex-quota-scheduler`. Resource routes may serve the UI
  shell and sanitized read-only status data only. Settings updates, imports,
  annotation writes, manual refresh, per-account refresh, and any future reset
  probe trigger must stay behind Management API routes protected by the CPA
  Management key.
- Keep `quota_endpoint` restricted to the fixed ChatGPT usage endpoint so Codex
  bearer credentials cannot be redirected to a user-controlled host.

## Testing

Unit tests should cover:

- default config keeps `EnableResetProbe` false;
- YAML/settings decode can enable and disable the checkbox;
- pending probe is created from an old matured reset even if a normal refresh
  overwrites quota with a lazy future reset;
- probe check is not due before `old_reset_at + 10m`;
- at `old_reset_at + 10m`, a real active window whose returned reset is roughly
  `now + window - 10m` does not send a probe;
- at `old_reset_at + 10m`, a lazy window whose returned reset is within 3
  minutes of `now + window` sends one probe;
- simultaneous five-hour and long-window resets preserve separate original reset
  baselines while sending at most one probe request for the account;
- successful probe requires usage evidence;
- successful probe requires usage evidence, then triggers a post-probe quota
  refresh attempt;
- a post-probe quota refresh failure is sanitized and recorded without sending a
  duplicate compact probe;
- failed probe stores a credential-redacted error, schedules a backoff retry, and
  does not tight-loop;
- retry exhaustion marks the window failed for that reset cycle;
- monthly windows without `limit_window_seconds` are not probed;
- monthly windows with `limit_window_seconds` use that exact duration;
- `NextRefreshDueAt` includes pending probe checks;
- management UI renders the warning outside the collapsed settings panel;
- legacy resource action queries cannot toggle `enable_reset_probe`, refresh
  quota, import configuration, or write annotations;
- public resource status data does not expose `quota_endpoint`, credential
  markers, management keys, or raw probe error details.

## Acceptance Criteria

- The new setting is visible in scheduling settings and defaults to off.
- A non-collapsed warning clearly explains that enabling the option sends a tiny
  Codex request.
- With the setting off, behavior is unchanged.
- With the setting on, the plugin waits 10 minutes after a cached reset before
  classifying lazy reset behavior.
- A real already-started window is not misclassified because the 10-minute delay
  is greater than the 3-minute threshold.
- A lazy reset is detected by comparing returned reset time with
  `now + limit_window_seconds`.
- Probe requests are sent through `host.http.do` per account.
- A successful probe is verified by usage evidence and followed by a quota
  refresh attempt.
- Monthly behavior relies on server-provided `limit_window_seconds`; no 30-day
  or calendar-month guess is used.
