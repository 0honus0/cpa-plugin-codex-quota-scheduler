# codex-quota-scheduler

Optimized Fill First scheduler for CLIProxyAPI Codex accounts.

## Build

```powershell
.\build.ps1
```

The build script requires Go with cgo support and a C compiler on `PATH` because
CPA loads this plugin as a Windows c-shared DLL. On Windows, install a toolchain
such as MinGW-w64 if `.\build.ps1` reports that `gcc` or the configured `CC`
cannot be found.

## Minimal CPA Config

```yaml
plugins:
  enabled: true
  configs:
    codex-quota-scheduler:
      enabled: true
      handle_enabled: true
      monthly_mode: expiry_order
      fallback: fill-first
      annotation_state_path: codex-quota-scheduler.annotations.json
      quota_refresh_interval: 30m
      stale_after: 6h
```

The plugin respects CPA auth priority first. Inside the active CPA priority tier,
it schedules by quota availability and reset or expiry time.

`monthly_mode` controls ordering for monthly accounts inside a CPA priority tier.
Use `expiry_order` to order by quota reset or expiry time, or `priority` to prefer
monthly accounts before weekly accounts within the same CPA priority.

## Management Routes

- `GET /v0/management/plugins/codex-quota-scheduler/status`
- `POST /v0/management/plugins/codex-quota-scheduler/refresh`
- `GET /v0/management/plugins/codex-quota-scheduler/annotations`
- `PUT /v0/management/plugins/codex-quota-scheduler/annotations`
- `PATCH /v0/management/plugins/codex-quota-scheduler/annotations/account`
- `PATCH /v0/management/plugins/codex-quota-scheduler/annotations/group`

## Manual Verification

1. Copy `dist/codex-quota-scheduler.dll` into CPA's plugin directory for the current platform.
2. Enable global plugins and this plugin in CPA config.
3. Start CPA and confirm `GET /v0/management/plugins` reports `registered: true` and `effective_enabled: true`.
4. Open `/v0/resource/plugins/codex-quota-scheduler/status`.
5. Confirm the status table follows scheduler order: CPA priority descending,
   then monthly mode ordering inside the priority tier. Unavailable accounts can
   appear before available accounts when they sort earlier.
6. Compare the `Next` value, or the top available account in the table, with the
   auth ID selected for the next Codex request.
7. Simulate or observe a 429 `usage_limit_reached` response and confirm the next scheduler pick avoids that account.
