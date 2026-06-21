# codex-quota-scheduler

Optimized Fill First scheduler for CLIProxyAPI Codex accounts.

## Build

```powershell
.\build.ps1
```

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
```

The plugin respects CPA auth priority first. Inside the active CPA priority tier,
it schedules by quota availability and reset or expiry time.
