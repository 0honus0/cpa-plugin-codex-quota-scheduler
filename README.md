# Codex Quota Scheduler

`codex-quota-scheduler` is a CLIProxyAPI (CPA) dynamic library plugin that
improves Codex account selection with an optimized Fill First scheduler.

The plugin keeps CPA's own auth priority as the first ordering rule. Within the
active CPA priority tier, it refreshes Codex quota status, tracks exhausted or
temporarily unavailable accounts, and picks the next account by availability and
reset or expiry time.

## Features

- Scheduler plugin for CPA Codex accounts.
- Usage feedback handling for `usage_limit_reached` responses.
- Five-hour, weekly, monthly, and reset-credit quota display when available.
- Circuit breaker state for repeated account failures.
- Bilingual Management UI: English and Chinese, with browser-language detection
  and a manual language selector.
- Account aliases, notes, tags, and groups stored in the plugin's local state.
- JSON export and import for scheduler settings and annotations.
- Release packaging for Linux, macOS, Windows, and FreeBSD.

## Privacy And Data Disclosure

This plugin runs inside CPA and uses CPA-provided host callbacks plus the CPA
plugin resource API. The Management UI does not require, store, or call a CPA
Management key, and it does not call the external CPA/VLink Management API. It
does not run an external service and does not send data to the plugin author.

The plugin may send authenticated requests to ChatGPT's quota and reset-credit
endpoints:

```text
GET https://chatgpt.com/backend-api/wham/usage
GET https://chatgpt.com/backend-api/wham/rate-limit-reset-credits
```

Those requests use the Codex credentials already configured in CPA. The plugin
uses the responses to calculate account quota state, reset-credit availability,
and scheduling order.

The plugin stores local state in CPA's plugin state area. Stored data can
include scheduler settings, recent quota snapshots, logs, aliases, notes, tags,
and group names. Do not put secrets in account notes, group notes, aliases, or
tags. The Management UI avoids rendering access tokens, authorization headers,
cookies, and other credential fields.

## Installation

Download the zip for your platform from the latest GitHub release:

```text
codex-quota-scheduler_<version>_<goos>_<goarch>.zip
```

Extract the dynamic library and place it in CPA's plugin directory for your
platform. The zip contains the library at the archive root:

- macOS: `codex-quota-scheduler.dylib`
- Linux and FreeBSD: `codex-quota-scheduler.so`
- Windows: `codex-quota-scheduler.dll`

Example:

```bash
mkdir -p /path/to/CLIProxyAPI/plugins/darwin/arm64
cp codex-quota-scheduler.dylib /path/to/CLIProxyAPI/plugins/darwin/arm64/
```

## CPA Configuration

Enable global plugins and this plugin in CPA:

```yaml
plugins:
  enabled: true
  configs:
    codex-quota-scheduler:
      enabled: true
      priority: 1
```

The plugin does not declare Management Center form fields. Scheduler settings,
aliases, notes, tags, and groups are edited from the plugin resource page and
persisted to the plugin state file.

The default scheduler settings are:

```yaml
handle_enabled: true
quota_refresh_interval: 30m
stale_after: 5h
monthly_mode: expiry_order
fallback: fill-first
enable_usage_feedback: true
max_refresh_concurrency: 1
quota_endpoint: https://chatgpt.com/backend-api/wham/usage
circuit_failure_threshold: 3
circuit_open_duration: 10m
circuit_half_open_success_threshold: 1
max_log_entries: 2000
log_retention: 24h
```

`monthly_mode` accepts:

- `expiry_order`: order monthly and weekly accounts by reset or expiry time
  within the same CPA priority tier.
- `priority`: prefer monthly accounts before weekly accounts within the same CPA
  priority tier.

## Management UI

Open the resource page from CPA's plugin resources, or visit:

```text
/v0/resource/plugins/codex-quota-scheduler/status
```

The page follows the browser language by default and can be switched between
English and Chinese manually. Settings, buttons, account cards, common status
text, and new UI log messages follow the selected language.

The page provides:

- Scheduler settings.
- Sorted account queue.
- Quota bars and reset times.
- Circuit breaker state.
- Account and group annotations.
- Log viewing and export.
- Configuration export and import.
- English and Chinese interface switching.

## Build

Requirements:

- Go 1.26 or newer, as declared by `go.mod`.
- CGO support.
- A C compiler for `-buildmode=c-shared`.
- `make` for the cross-platform release workflow.

Run tests:

```bash
make test
```

Build the dynamic library for the current platform:

```bash
make build
```

Build and package the release zip:

```bash
make package VERSION=0.1.0
```

Generate an aggregate checksum file for local release assets:

```bash
make checksums VERSION=0.1.0
```

Windows users can also use the PowerShell helper:

```powershell
.\build.ps1
```

The PowerShell script builds `dist/codex-quota-scheduler.dll` and requires a C
compiler such as MinGW-w64 on `PATH`.

## GitHub Releases

Version `0.1.0` is the first public release version for this repository.
GitHub Actions builds release assets when a tag matching `v*` is pushed. Use a
dotted numeric version tag such as:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `Build` workflow runs tests and creates the release automatically. Release
assets are named:

```text
codex-quota-scheduler_<version>_<goos>_<goarch>.zip
checksums.txt
```

For `v0.1.0`, the expected platform assets are:

- `codex-quota-scheduler_0.1.0_darwin_amd64.zip`
- `codex-quota-scheduler_0.1.0_darwin_arm64.zip`
- `codex-quota-scheduler_0.1.0_freebsd_amd64.zip`
- `codex-quota-scheduler_0.1.0_linux_amd64.zip`
- `codex-quota-scheduler_0.1.0_linux_arm64.zip`
- `codex-quota-scheduler_0.1.0_windows_amd64.zip`
- `codex-quota-scheduler_0.1.0_windows_arm64.zip`
- `checksums.txt`

`checksums.txt` uses sha256sum format:

```text
<sha256>  codex-quota-scheduler_0.1.0_darwin_arm64.zip
```

## Management API

The Management UI uses the CPA plugin resource API instead of the CPA
Management HTTP API. All page actions are sent as `GET` requests to:

```text
GET /v0/resource/plugins/codex-quota-scheduler/status?action=<action>
```

Actions that modify data include a JSON `payload` query parameter. Supported
actions are:

- `settings`: update scheduler settings.
- `refresh`: request quota refresh for all accounts.
- `refresh_account`: request quota refresh for one account.
- `logs`: read recent scheduler logs.
- `export`: export scheduler settings and annotations.
- `import`: import scheduler settings and annotations.
- `annotations`: read account and group annotations.
- `annotations_replace`: replace all annotations.
- `annotations_account`: update one account annotation.
- `annotations_group`: update one group annotation.

CPA host integrations may still list the plugin's management routes for
compatibility, but the bundled UI does not depend on a Management key.

## License

MIT License. See [LICENSE](LICENSE).
