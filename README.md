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
```

The plugin respects CPA auth priority first. Inside the active CPA priority tier,
it schedules by quota availability and reset or expiry time.

The plugin does not declare Management Center configuration fields. Scheduler
settings, aliases, notes, tags, and groups are edited from the plugin resource
page and persisted to the plugin's built-in state file.

`monthly_mode` can be changed from the plugin page. Use `expiry_order` to order
by quota reset or expiry time, or `priority` to prefer monthly accounts before
weekly accounts within the same CPA priority.

## Management Routes

- `GET /v0/management/plugins/codex-quota-scheduler/status`
- `GET /v0/management/plugins/codex-quota-scheduler/settings`
- `PUT /v0/management/plugins/codex-quota-scheduler/settings`
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
5. Confirm the account cards follow scheduler order: CPA priority descending,
   then monthly mode ordering inside the priority tier. Unavailable accounts can
   appear before available accounts when they sort earlier.
6. Compare the `Next` value, or the top available account card, with the
   auth ID selected for the next Codex request.
7. Simulate or observe a 429 `usage_limit_reached` response and confirm the next scheduler pick avoids that account.
