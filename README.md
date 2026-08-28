# Codex Quota Scheduler

A CLIProxyAPI (CPA) plugin for Codex account scheduling and quota maintenance. v0.3.0 is a breaking rewrite: the legacy admission/refresher/roster/WAL/management implementation has been removed.

## Rules

- CPA priority is the sole authority across priority tiers.
- The plugin only orders candidates inside the same CPA priority tier.
- All CPA-enabled Codex accounts appear in the management UI, regardless of priority.
- Background quota refresh is enabled per account by default and can be disabled independently without changing CPA priority.
- Normal user/API request bodies are never rewritten by this plugin.

Within one CPA priority tier, `scheduler_priority` is considered first, then cached quota, then stable account ID.

## Refresh and lazy 5-hour reset

Quota refresh uses `GET https://chatgpt.com/backend-api/wham/usage`.

For a known five-hour `reset_at`, the default flow is: wait until reset, wait another 5 minutes, refresh quota, and only if the old window is still present send the existing minimal probe to `POST https://chatgpt.com/backend-api/codex/responses`. The probe payload is intentionally preserved byte-for-byte. Quota is checked again about 3 seconds later.

## 401 / 429

- 429 records Retry-After/recovery information and temporarily avoids that account inside the current CPA tier. CPA's native cooldown remains authoritative for cross-tier fallback.
- 401 sets the CPA auth `disabled` flag. Only that top-level flag is changed; credential fields are preserved. A credential fingerprint change caused by re-login/token replacement allows the plugin to re-enable the auth.

## Security

The plugin does not persist raw access/refresh/ID tokens and does not log Authorization/Cookie/token material. Credential mutation is intentionally limited to toggling the CPA auth `disabled` flag. Management responses are no-store and the UI uses a restrictive baseline CSP.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    codex-quota-scheduler:
      enabled: true
      priority: 1
      handle_enabled: true
      quota_refresh_interval: 30m
      quota_stale_after: 5h
      enable_reset_probe: true
      reset_probe_after_reset_delay: 5m
      reset_probe_retry_delay: 5m
      autoban_429: true
      disable_on_401: true
```

CPA account priorities may remain different (for example 9 and 8). The plugin never uses its own priority to jump across those CPA tiers.

## Build

Go 1.26 and a C compiler are required:

```bash
make test
make vet
make build VERSION=0.3.0
```

Install the resulting platform library in CPA's matching plugin directory and restart CPA.
