# Codex Quota Scheduler Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a CLIProxyAPI Go dynamic-library plugin that schedules Codex accounts by CPA priority first, then quota availability and reset/expiry ordering.

**Architecture:** Keep the plugin core testable without loading CPA: config parsing, quota parsing, annotation persistence, usage feedback, and scheduler ordering live in ordinary Go files with unit tests. The C ABI shell only dispatches `plugin.register`, `plugin.reconfigure`, `scheduler.pick`, `usage.handle`, `management.register`, and `management.handle` to the core services.

**Tech Stack:** Go 1.26, CGO `-buildmode=c-shared`, CLIProxyAPI `sdk/pluginapi` and `sdk/pluginabi`, `gopkg.in/yaml.v3`, standard-library HTML/JSON/HTTP/time packages.

---

## File Structure

Project root: `C:\Users\Jeffery\Desktop\Projects\Personal\CPA-Plugin\codex-quota-scheduler`

- Create `go.mod`: module metadata with local `replace` to the checked-out CPA repository.
- Create `main.go`: C ABI entry points, host callback bridge, method dispatch, envelope helpers, lifecycle wiring.
- Create `config.go`: YAML config decoding, default values, duration parsing, config field metadata.
- Create `models.go`: core domain types for quota windows, account state, annotations, scheduler snapshots, and status output.
- Create `auth.go`: Codex auth JSON and JWT claim extraction.
- Create `quota.go`: `wham/usage` payload parsing and window classification.
- Create `state.go`: concurrency-safe plugin state, refresh snapshots, annotations, selection history.
- Create `annotations.go`: annotation key resolution and JSON persistence.
- Create `scheduler.go`: CPA-priority-first scheduling logic and status ordering.
- Create `usage.go`: quota-like failure detection and immediate state updates.
- Create `refresh.go`: host auth/http callback calls and background refresh loop.
- Create `management.go`: management route registration, status JSON, annotation endpoints, and status HTML.
- Create `build.ps1`: local Windows build helper for `codex-quota-scheduler.dll`.
- Create `README.md`: local build, config, and operational notes.
- Create tests beside each Go file using `*_test.go`.

Keep all files in `package main`. This matches CPA examples and lets unit tests exercise unexported helpers without adding an internal package too early.

## Task 1: Bootstrap Plugin Module, Config, And Registration

**Files:**
- Create: `go.mod`
- Create: `main.go`
- Create: `config.go`
- Create: `config_test.go`
- Create: `build.ps1`
- Create: `README.md`

- [ ] **Step 1: Write failing config tests**

Create `config_test.go` with:

```go
package main

import (
	"testing"
	"time"
)

func TestDecodeConfigDefaults(t *testing.T) {
	cfg, err := DecodeConfig(nil)
	if err != nil {
		t.Fatalf("DecodeConfig returned error: %v", err)
	}
	if !cfg.HandleEnabled {
		t.Fatalf("HandleEnabled = false, want true")
	}
	if cfg.MonthlyMode != MonthlyModeExpiryOrder {
		t.Fatalf("MonthlyMode = %q, want %q", cfg.MonthlyMode, MonthlyModeExpiryOrder)
	}
	if cfg.Fallback != FallbackFillFirst {
		t.Fatalf("Fallback = %q, want %q", cfg.Fallback, FallbackFillFirst)
	}
	if cfg.QuotaRefreshInterval != time.Minute {
		t.Fatalf("QuotaRefreshInterval = %s, want 1m", cfg.QuotaRefreshInterval)
	}
	if cfg.StaleAfter != 10*time.Minute {
		t.Fatalf("StaleAfter = %s, want 10m", cfg.StaleAfter)
	}
}

func TestDecodeConfigOverrides(t *testing.T) {
	raw := []byte(`
handle_enabled: false
quota_refresh_interval: 30s
stale_after: 2m
monthly_mode: priority
fallback: fill-first
enable_usage_feedback: false
annotation_state_path: C:\state\annotations.json
max_refresh_concurrency: 8
quota_endpoint: https://example.test/usage
`)
	cfg, err := DecodeConfig(raw)
	if err != nil {
		t.Fatalf("DecodeConfig returned error: %v", err)
	}
	if cfg.HandleEnabled {
		t.Fatalf("HandleEnabled = true, want false")
	}
	if cfg.MonthlyMode != MonthlyModePriority {
		t.Fatalf("MonthlyMode = %q, want %q", cfg.MonthlyMode, MonthlyModePriority)
	}
	if cfg.EnableUsageFeedback {
		t.Fatalf("EnableUsageFeedback = true, want false")
	}
	if cfg.MaxRefreshConcurrency != 8 {
		t.Fatalf("MaxRefreshConcurrency = %d, want 8", cfg.MaxRefreshConcurrency)
	}
}

func TestDecodeConfigRejectsInvalidMonthlyMode(t *testing.T) {
	_, err := DecodeConfig([]byte("monthly_mode: unsupported\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted invalid monthly mode")
	}
}

func TestPluginRegistrationDeclaresCapabilitiesAndFields(t *testing.T) {
	reg := PluginRegistration()
	if reg.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", reg.SchemaVersion)
	}
	if !reg.Capabilities.Scheduler || !reg.Capabilities.UsagePlugin || !reg.Capabilities.ManagementAPI {
		t.Fatalf("capabilities = %#v, want scheduler, usage_plugin, management_api", reg.Capabilities)
	}
	names := map[string]bool{}
	for _, field := range reg.Metadata.ConfigFields {
		names[field.Name] = true
	}
	for _, name := range []string{"handle_enabled", "monthly_mode", "quota_refresh_interval", "stale_after", "enable_usage_feedback"} {
		if !names[name] {
			t.Fatalf("ConfigFields missing %s", name)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```powershell
go test ./...
```

Expected: FAIL because no Go module and no config types exist.

- [ ] **Step 3: Create `go.mod`**

```go
module github.com/jeffery/codex-quota-scheduler

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v7 v7.0.0
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/router-for-me/CLIProxyAPI/v7 => ../CLIProxyAPI
```

- [ ] **Step 4: Implement config and registration**

Create `config.go` with `Config`, `MonthlyMode`, `FallbackMode`, `DecodeConfig`, `DefaultConfig`, `PluginRegistration`, and `ConfigFields`.

Required behavior:

```go
const (
	PluginID = "codex-quota-scheduler"

	MonthlyModePriority    MonthlyMode = "priority"
	MonthlyModeExpiryOrder MonthlyMode = "expiry_order"

	FallbackFillFirst FallbackMode = "fill-first"
)
```

`DefaultConfig()` must return:

```go
Config{
	HandleEnabled:        true,
	QuotaRefreshInterval: time.Minute,
	StaleAfter:           10 * time.Minute,
	MonthlyMode:          MonthlyModeExpiryOrder,
	Fallback:             FallbackFillFirst,
	EnableUsageFeedback:  true,
	MaxRefreshConcurrency: 4,
	QuotaEndpoint:        "https://chatgpt.com/backend-api/wham/usage",
}
```

`DecodeConfig` must parse duration strings with `time.ParseDuration`, reject `monthly_mode` values other than `priority` or `expiry_order`, and reject fallback values other than `fill-first` or empty string.

`PluginRegistration` must return metadata name `codex-quota-scheduler`, version `0.1.0`, and capabilities:

```go
type registrationCapabilities struct {
	Scheduler     bool `json:"scheduler"`
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}
```

- [ ] **Step 5: Implement ABI skeleton and lifecycle dispatch**

Create `main.go` based on CPA's Go examples:

- Export `cliproxy_plugin_init`.
- Export `cliproxyPluginCall`.
- Export `cliproxyPluginFree`.
- Export `cliproxyPluginShutdown`.
- Store host API pointers for future callbacks.
- Decode lifecycle requests with `ConfigYAML []byte`.
- On `plugin.register` and `plugin.reconfigure`, call `DecodeConfig`, store it in global plugin state, and return `PluginRegistration`.
- Return `unknown_method` envelope for unhandled methods.

At this task, `scheduler.pick`, `usage.handle`, and `management.*` can return valid empty responses:

```go
case pluginabi.MethodSchedulerPick:
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
case pluginabi.MethodUsageHandle:
	return okEnvelope(map[string]any{})
case pluginabi.MethodManagementRegister:
	return okEnvelope(pluginapi.ManagementRegistrationResponse{})
case pluginabi.MethodManagementHandle:
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusNotFound})
```

- [ ] **Step 6: Add build helper and README**

Create `build.ps1`:

```powershell
$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force dist | Out-Null
go test ./...
go build -buildmode=c-shared -o dist/codex-quota-scheduler.dll .
```

Create `README.md` with:

````markdown
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
````

- [ ] **Step 7: Run tests**

Run:

```powershell
go test ./...
```

Expected: PASS.

- [ ] **Step 8: Commit**

```powershell
git add go.mod main.go config.go config_test.go build.ps1 README.md
git commit -m "feat: bootstrap plugin module"
```

## Task 2: Auth Extraction And Quota Parsing

**Files:**
- Create: `models.go`
- Create: `auth.go`
- Create: `auth_test.go`
- Create: `quota.go`
- Create: `quota_test.go`

- [ ] **Step 1: Write failing auth extraction tests**

Create `auth_test.go` with:

```go
package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func makeUnsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return strings.Join([]string{header, payload, ""}, ".")
}

func TestExtractCodexCredentialsTopLevel(t *testing.T) {
	idToken := makeUnsignedJWT(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_123",
		},
	})
	raw := json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`)
	creds, err := ExtractCodexCredentials(raw)
	if err != nil {
		t.Fatalf("ExtractCodexCredentials returned error: %v", err)
	}
	if creds.AccessToken != "access-1" || creds.ChatGPTAccountID != "acct_123" {
		t.Fatalf("creds = %#v", creds)
	}
}

func TestExtractCodexCredentialsNestedMetadata(t *testing.T) {
	idToken := makeUnsignedJWT(t, map[string]any{
		"https://api.openai.com/auth.chatgpt_account_id": "acct_nested",
	})
	raw := json.RawMessage(`{"tokens":{"access_token":"access-2"},"metadata":{"id_token":"` + idToken + `"}}`)
	creds, err := ExtractCodexCredentials(raw)
	if err != nil {
		t.Fatalf("ExtractCodexCredentials returned error: %v", err)
	}
	if creds.AccessToken != "access-2" || creds.ChatGPTAccountID != "acct_nested" {
		t.Fatalf("creds = %#v", creds)
	}
}

func TestExtractCodexCredentialsRejectsMissingAccessToken(t *testing.T) {
	_, err := ExtractCodexCredentials(json.RawMessage(`{"id_token":"x.y.z"}`))
	if err == nil {
		t.Fatalf("ExtractCodexCredentials accepted missing access token")
	}
}
```

- [ ] **Step 2: Write failing quota parser tests**

Create `quota_test.go` with:

```go
package main

import (
	"testing"
	"time"
)

func TestParseCodexUsageWeeklyWindows(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "plan_type": "plus",
	  "rate_limit": {
	    "allowed": true,
	    "primary_window": {"used_percent": 20, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
	    "secondary_window": {"used_percent": 55, "limit_window_seconds": 604800, "reset_after_seconds": 86400}
	  },
	  "rate_limit_reset_credits": {"available_count": 2}
	}`)
	parsed, err := ParseCodexUsagePayload(raw, now)
	if err != nil {
		t.Fatalf("ParseCodexUsagePayload returned error: %v", err)
	}
	if parsed.Family != AccountFamilyWeekly {
		t.Fatalf("Family = %q, want weekly", parsed.Family)
	}
	if parsed.FiveHour == nil || parsed.LongWindow == nil {
		t.Fatalf("missing windows: %#v", parsed)
	}
	if !parsed.LongWindow.ResetAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("weekly reset = %s, want %s", parsed.LongWindow.ResetAt, now.Add(24*time.Hour))
	}
	if parsed.ResetCreditsAvailableCount == nil || *parsed.ResetCreditsAvailableCount != 2 {
		t.Fatalf("reset credits = %#v", parsed.ResetCreditsAvailableCount)
	}
}

func TestParseCodexUsageMonthlyWindow(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "rate_limit": {
	    "primary_window": {"used_percent": 10, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
	    "secondary_window": {"used_percent": 30, "limit_window_seconds": 2592000, "reset_after_seconds": 172800}
	  }
	}`)
	parsed, err := ParseCodexUsagePayload(raw, now)
	if err != nil {
		t.Fatalf("ParseCodexUsagePayload returned error: %v", err)
	}
	if parsed.Family != AccountFamilyMonthly {
		t.Fatalf("Family = %q, want monthly", parsed.Family)
	}
	if !parsed.LongWindow.ResetAt.Equal(now.Add(48 * time.Hour)) {
		t.Fatalf("monthly reset = %s, want %s", parsed.LongWindow.ResetAt, now.Add(48*time.Hour))
	}
}

func TestParseCodexUsageExhaustedFromAllowedFalse(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "rate_limit": {
	    "allowed": false,
	    "limit_reached": true,
	    "primary_window": {"used_percent": 100, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
	    "secondary_window": {"used_percent": 20, "limit_window_seconds": 604800, "reset_after_seconds": 86400}
	  }
	}`)
	parsed, err := ParseCodexUsagePayload(raw, now)
	if err != nil {
		t.Fatalf("ParseCodexUsagePayload returned error: %v", err)
	}
	if parsed.FiveHour == nil || !parsed.FiveHour.Exhausted {
		t.Fatalf("five hour window not exhausted: %#v", parsed.FiveHour)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run:

```powershell
go test ./...
```

Expected: FAIL because `ExtractCodexCredentials`, `ParseCodexUsagePayload`, and domain types do not exist.

- [ ] **Step 4: Implement domain models**

Create `models.go` with these core types:

```go
type AccountFamily string

const (
	AccountFamilyUnknown AccountFamily = "unknown"
	AccountFamilyWeekly  AccountFamily = "weekly"
	AccountFamilyMonthly AccountFamily = "monthly"
)

type WindowKind string

const (
	WindowFiveHour WindowKind = "five_hour"
	WindowWeekly   WindowKind = "weekly"
	WindowMonthly  WindowKind = "monthly"
)

type QuotaWindow struct {
	Kind               WindowKind `json:"kind"`
	UsedPercent        *float64   `json:"used_percent,omitempty"`
	LimitWindowSeconds *int64     `json:"limit_window_seconds,omitempty"`
	ResetAt            time.Time  `json:"reset_at"`
	Exhausted          bool       `json:"exhausted"`
}

type ParsedQuota struct {
	PlanType                       string       `json:"plan_type,omitempty"`
	Family                         AccountFamily `json:"family"`
	FiveHour                       *QuotaWindow `json:"five_hour,omitempty"`
	LongWindow                     *QuotaWindow `json:"long_window,omitempty"`
	CodeReviewWindows              []QuotaWindow `json:"code_review_windows,omitempty"`
	AdditionalWindows              []QuotaWindow `json:"additional_windows,omitempty"`
	ResetCreditsAvailableCount     *int         `json:"reset_credits_available_count,omitempty"`
}
```

Use imports `time` in `models.go`.

- [ ] **Step 5: Implement auth extraction**

Create `auth.go` with:

- `type CodexCredentials struct { AccessToken string; IDToken string; ChatGPTAccountID string }`
- `ExtractCodexCredentials(raw json.RawMessage) (CodexCredentials, error)`
- recursive string lookup for `access_token` and `id_token`
- JWT payload decoding with `base64.RawURLEncoding`
- account ID extraction from:
  - claim `https://api.openai.com/auth.chatgpt_account_id`
  - nested claim `https://api.openai.com/auth.chatgpt_account_id`
  - nested claim object `https://api.openai.com/auth` containing `chatgpt_account_id`
  - fallback claim `chatgpt_account_id`

Return errors with these stable substrings:

- `missing access_token`
- `missing chatgpt_account_id`

- [ ] **Step 6: Implement quota parsing**

Create `quota.go` with:

- constants `fiveHourSeconds = 18000`, `weekSeconds = 604800`, `minMonthSeconds = 2419200`, `maxMonthSeconds = 2678400`
- `ParseCodexUsagePayload(raw []byte, now time.Time) (ParsedQuota, error)`
- tolerant snake_case and camelCase parsing for fields used by Management Center:
  - `plan_type` / `planType`
  - `rate_limit` / `rateLimit`
  - `primary_window` / `primaryWindow`
  - `secondary_window` / `secondaryWindow`
  - `used_percent` / `usedPercent`
  - `limit_window_seconds` / `limitWindowSeconds`
  - `reset_after_seconds` / `resetAfterSeconds`
  - `reset_at` / `resetAt`
  - `allowed`
  - `limit_reached` / `limitReached`
  - `rate_limit_reset_credits.available_count` / `availableCount`
- classify primary/secondary by `limit_window_seconds`, with primary/secondary order fallback when seconds are absent
- compute `ResetAt` from `reset_at` when present, otherwise `now + reset_after_seconds`
- mark a window exhausted when `used_percent >= 100`, `limit_reached == true`, or `allowed == false`

- [ ] **Step 7: Run tests**

Run:

```powershell
go test ./...
```

Expected: PASS.

- [ ] **Step 8: Commit**

```powershell
git add models.go auth.go auth_test.go quota.go quota_test.go
git commit -m "feat: parse codex auth and quota"
```

## Task 3: State Store And Annotation Persistence

**Files:**
- Create: `state.go`
- Create: `state_test.go`
- Create: `annotations.go`
- Create: `annotations_test.go`
- Modify: `models.go`

- [ ] **Step 1: Write failing annotation tests**

Create `annotations_test.go` with:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAnnotationKeyPrefersAuthIDThenChatGPTAccountID(t *testing.T) {
	acct := AccountState{AuthID: "auth-1", ChatGPTAccountID: "acct-1", Email: "a@example.com"}
	if got := ResolveAnnotationKey(acct); got != "auth:auth-1" {
		t.Fatalf("key = %q, want auth:auth-1", got)
	}
	acct.AuthID = ""
	if got := ResolveAnnotationKey(acct); got != "chatgpt:acct-1" {
		t.Fatalf("key = %q, want chatgpt:acct-1", got)
	}
	acct.ChatGPTAccountID = ""
	if got := ResolveAnnotationKey(acct); got != "email:a@example.com" {
		t.Fatalf("key = %q, want email:a@example.com", got)
	}
}

func TestAnnotationStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "annotations.json")
	state := AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "Team A 01", Notes: "shared", Tags: []string{"team-a"}, GroupID: "team-a"},
		},
		Groups: map[string]GroupAnnotation{
			"team-a": {Name: "Team A", Notes: "weekly pool", Tags: []string{"team"}, Color: "#2563eb"},
		},
	}
	if err := SaveAnnotations(path, state); err != nil {
		t.Fatalf("SaveAnnotations returned error: %v", err)
	}
	loaded, err := LoadAnnotations(path)
	if err != nil {
		t.Fatalf("LoadAnnotations returned error: %v", err)
	}
	if loaded.Accounts["auth:auth-1"].Alias != "Team A 01" {
		t.Fatalf("loaded annotation = %#v", loaded.Accounts["auth:auth-1"])
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if len(raw) == 0 || raw[0] != '{' {
		t.Fatalf("annotation file is not JSON object: %q", raw)
	}
}
```

- [ ] **Step 2: Write failing state tests**

Create `state_test.go` with:

```go
package main

import (
	"testing"
	"time"
)

func TestPluginStateUpsertsAndSnapshotsAccounts(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{
		AuthID:    "auth-1",
		AuthIndex: "idx-1",
		Provider:  "codex",
		Priority:  5,
		Family:    AccountFamilyWeekly,
		Quota: ParsedQuota{
			Family: AccountFamilyWeekly,
			FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: now.Add(time.Hour)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(24 * time.Hour)},
		},
		LastRefreshAt: now,
		LastSuccessAt: now,
	})
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	if snapshot.Accounts[0].AuthID != "auth-1" {
		t.Fatalf("account = %#v", snapshot.Accounts[0])
	}
}

func TestPluginStateMarksStale(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StaleAfter = time.Minute
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{AuthID: "auth-1", LastSuccessAt: now.Add(-2 * time.Minute)})
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 || !snapshot.Accounts[0].Stale {
		t.Fatalf("snapshot = %#v", snapshot.Accounts)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run:

```powershell
go test ./...
```

Expected: FAIL because state and annotation functions do not exist.

- [ ] **Step 4: Extend `models.go`**

Add:

```go
type AccountAnnotation struct {
	Alias   string   `json:"alias,omitempty" yaml:"alias,omitempty"`
	Notes   string   `json:"notes,omitempty" yaml:"notes,omitempty"`
	Tags    []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	GroupID string   `json:"group_id,omitempty" yaml:"group_id,omitempty"`
}

type GroupAnnotation struct {
	Name  string   `json:"name,omitempty" yaml:"name,omitempty"`
	Notes string   `json:"notes,omitempty" yaml:"notes,omitempty"`
	Tags  []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Color string   `json:"color,omitempty" yaml:"color,omitempty"`
}

type AnnotationState struct {
	Accounts map[string]AccountAnnotation `json:"accounts,omitempty" yaml:"accounts,omitempty"`
	Groups   map[string]GroupAnnotation   `json:"groups,omitempty" yaml:"groups,omitempty"`
}

type AccountState struct {
	AuthID             string
	AuthIndex          string
	DisplayName        string
	Email              string
	Provider           string
	Priority           int
	ChatGPTAccountID   string
	Family             AccountFamily
	Quota              ParsedQuota
	LastRefreshAt      time.Time
	LastSuccessAt      time.Time
	LastError          string
	Stale              bool
	TemporaryExhausted bool
	TemporaryResetAt   time.Time
	Annotation         AccountAnnotation
}

type StateSnapshot struct {
	Config      Config
	Accounts    []AccountState
	Annotations AnnotationState
	LastSelected string
	LastReason   string
	Now          time.Time
}
```

- [ ] **Step 5: Implement annotation persistence**

Create `annotations.go`:

- `NormalizeAnnotationState(state AnnotationState) AnnotationState`: ensures non-nil maps and trims duplicate empty tags.
- `ResolveAnnotationKey(account AccountState) string`: returns `auth:<auth_id>`, then `chatgpt:<account_id>`, then `email:<email>`, then `index:<auth_index>`.
- `LoadAnnotations(path string) (AnnotationState, error)`: missing file returns empty normalized state.
- `SaveAnnotations(path string, state AnnotationState) error`: creates parent directory and writes indented JSON with mode `0600`.
- `ApplyAnnotations(accounts []AccountState, state AnnotationState) []AccountState`: attaches per-account annotation by resolved key.

- [ ] **Step 6: Implement concurrency-safe plugin state**

Create `state.go`:

- `type PluginState struct { mu sync.RWMutex; cfg Config; accounts map[string]AccountState; annotations AnnotationState; lastSelected string; lastReason string }`
- `NewPluginState(cfg Config) *PluginState`
- `ReplaceConfig(cfg Config)`
- `Config() Config`
- `SetAnnotations(state AnnotationState)`
- `Annotations() AnnotationState`
- `UpsertQuota(account AccountState)`
- `MarkAccountTemporaryExhausted(authID string, resetAt time.Time, reason string)`
- `RecordSelection(authID, reason string)`
- `Snapshot(now time.Time) StateSnapshot`

`Snapshot` must clone maps/slices and compute `Stale` by `now.Sub(LastSuccessAt) > cfg.StaleAfter` when `LastSuccessAt` is non-zero.

- [ ] **Step 7: Run tests**

Run:

```powershell
go test ./...
```

Expected: PASS.

- [ ] **Step 8: Commit**

```powershell
git add models.go state.go state_test.go annotations.go annotations_test.go
git commit -m "feat: add quota state and annotations"
```

## Task 4: Scheduler Ordering Engine

**Files:**
- Create: `scheduler.go`
- Create: `scheduler_test.go`
- Modify: `main.go`

- [ ] **Step 1: Write failing scheduler tests**

Create `scheduler_test.go` with:

```go
package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func weeklyAccount(id string, priority int, weeklyReset time.Time, fiveHourExhausted bool) AccountState {
	usedFive := 10.0
	if fiveHourExhausted {
		usedFive = 100
	}
	return AccountState{
		AuthID:   id,
		Provider: "codex",
		Priority: priority,
		Family:   AccountFamilyWeekly,
		Quota: ParsedQuota{
			Family: AccountFamilyWeekly,
			FiveHour: &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &usedFive, ResetAt: weeklyReset.Add(-20 * time.Hour), Exhausted: fiveHourExhausted},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: weeklyReset},
		},
		LastSuccessAt: weeklyReset.Add(-48 * time.Hour),
	}
}

func monthlyAccount(id string, priority int, monthlyReset time.Time) AccountState {
	used := 20.0
	return AccountState{
		AuthID:   id,
		Provider: "codex",
		Priority: priority,
		Family:   AccountFamilyMonthly,
		Quota: ParsedQuota{
			Family: AccountFamilyMonthly,
			LongWindow: &QuotaWindow{Kind: WindowMonthly, UsedPercent: &used, ResetAt: monthlyReset},
		},
		LastSuccessAt: monthlyReset.Add(-48 * time.Hour),
	}
}

func requestWithCandidates(ids ...string) pluginapi.SchedulerPickRequest {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Status: "active"})
	}
	return pluginapi.SchedulerPickRequest{Provider: "codex", Providers: []string{"codex"}, Candidates: candidates}
}

func TestPickRespectsCPAPriorityBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModeExpiryOrder
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("low-earliest", 1, now.Add(1*time.Hour), false),
		weeklyAccount("high-later", 10, now.Add(72*time.Hour), false),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "low-earliest", Provider: "codex", Priority: 1, Status: "active"},
			{ID: "high-later", Provider: "codex", Priority: 10, Status: "active"},
		},
	}
	decision := PickCodexAccount(req, snapshot, now)
	if !decision.Handled || decision.AuthID != "high-later" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestPickWeeklyEarliestResetWithinPriority(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("later", 5, now.Add(48*time.Hour), false),
		weeklyAccount("earlier", 5, now.Add(24*time.Hour), false),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "later", Provider: "codex", Priority: 5, Status: "active"},
			{ID: "earlier", Provider: "codex", Priority: 5, Status: "active"},
		},
	}
	decision := PickCodexAccount(req, snapshot, now)
	if decision.AuthID != "earlier" {
		t.Fatalf("AuthID = %q, want earlier", decision.AuthID)
	}
}

func TestPickMonthlyPriorityMode(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModePriority
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("weekly-soon", 5, now.Add(1*time.Hour), false),
		monthlyAccount("monthly-later", 5, now.Add(72*time.Hour)),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "weekly-soon", Provider: "codex", Priority: 5, Status: "active"},
			{ID: "monthly-later", Provider: "codex", Priority: 5, Status: "active"},
		},
	}
	decision := PickCodexAccount(req, snapshot, now)
	if decision.AuthID != "monthly-later" {
		t.Fatalf("AuthID = %q, want monthly-later", decision.AuthID)
	}
}

func TestPickMonthlyExpiryOrderMode(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModeExpiryOrder
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("weekly-soon", 5, now.Add(1*time.Hour), false),
		monthlyAccount("monthly-later", 5, now.Add(72*time.Hour)),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "weekly-soon", Provider: "codex", Priority: 5, Status: "active"},
			{ID: "monthly-later", Provider: "codex", Priority: 5, Status: "active"},
		},
	}
	decision := PickCodexAccount(req, snapshot, now)
	if decision.AuthID != "weekly-soon" {
		t.Fatalf("AuthID = %q, want weekly-soon", decision.AuthID)
	}
}

func TestPickSkipsWeeklyWhenFiveHourExhausted(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("blocked", 5, now.Add(time.Hour), true),
		weeklyAccount("available", 5, now.Add(2*time.Hour), false),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "blocked", Provider: "codex", Priority: 5, Status: "active"},
			{ID: "available", Provider: "codex", Priority: 5, Status: "active"},
		},
	}
	decision := PickCodexAccount(req, snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available", decision.AuthID)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```powershell
go test ./...
```

Expected: FAIL because `PickCodexAccount` does not exist.

- [ ] **Step 3: Implement scheduler engine**

Create `scheduler.go` with:

- `type PickDecision struct { AuthID string; Handled bool; DelegateBuiltin string; Reason string; Ordered []ScheduledAccount }`
- `type ScheduledAccount struct { AuthID string; Priority int; Family AccountFamily; Available bool; UnavailableReason string; SortTime time.Time; Annotation AccountAnnotation }`
- `PickCodexAccount(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time) PickDecision`
- `BuildOrderedAccounts(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time) []ScheduledAccount`
- `accountAvailable(account AccountState, now time.Time) (bool, string)`

Rules:

- Return `Handled=false` when `snapshot.Config.HandleEnabled == false`.
- Return `Handled=false` when request provider and providers do not include `codex`.
- Consider only candidate IDs from `req.Candidates`.
- Use candidate priority as source of truth; copy candidate priority into ordering even if cached account priority differs.
- Sort priority tiers descending.
- Within one tier, remove unavailable/stale/unknown accounts from selection but keep them in status ordering with an unavailable reason.
- In `monthly_mode: priority`, sort monthly accounts before weekly accounts, each by long-window reset time.
- In `monthly_mode: expiry_order`, sort weekly and monthly accounts together by long-window reset time.
- Break ties by `AuthID`.
- When no account is selectable for a Codex request and `cfg.Fallback == fill-first`, return `Handled=true` and `DelegateBuiltin=pluginapi.SchedulerBuiltinFillFirst`.
- When no account is selectable and fallback is empty, return `Handled=false`.

- [ ] **Step 4: Wire scheduler dispatch in `main.go`**

Add global:

```go
var globalState = NewPluginState(DefaultConfig())
```

In lifecycle configure, call:

```go
globalState.ReplaceConfig(cfg)
```

For `scheduler.pick`, decode `pluginapi.SchedulerPickRequest`, call `globalState.Snapshot(time.Now())`, pass to `PickCodexAccount`, record selection when `AuthID` is non-empty, and return `pluginapi.SchedulerPickResponse`.

- [ ] **Step 5: Run tests**

Run:

```powershell
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add scheduler.go scheduler_test.go main.go
git commit -m "feat: schedule codex accounts by priority and expiry"
```

## Task 5: Usage Failure Feedback

**Files:**
- Create: `usage.go`
- Create: `usage_test.go`
- Modify: `main.go`

- [ ] **Step 1: Write failing usage feedback tests**

Create `usage_test.go` with:

```go
package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDetectQuotaFailureUsageLimitReachedWithResetAt(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body: `{"error":{"type":"usage_limit_reached","message":"limit","resets_at":"2026-06-21T12:00:00Z"}}`,
		},
	}
	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if event.AuthID != "auth-1" || !event.ResetAt.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("event = %#v", event)
	}
}

func TestDetectQuotaFailureResetsInSeconds(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body: `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`,
		},
	}
	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(now.Add(120 * time.Second)) {
		t.Fatalf("ResetAt = %s, want %s", event.ResetAt, now.Add(120*time.Second))
	}
}

func TestDetectQuotaFailureIgnoresGenericRateLimit(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body: `{"error":{"type":"rate_limit_error"}}`,
		},
	}
	if event, ok := DetectQuotaFailure(record, now); ok {
		t.Fatalf("unexpected event = %#v", event)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```powershell
go test ./...
```

Expected: FAIL because `DetectQuotaFailure` does not exist.

- [ ] **Step 3: Implement usage feedback**

Create `usage.go` with:

- `type QuotaFailureEvent struct { AuthID string; AuthIndex string; ResetAt time.Time; Reason string }`
- `DetectQuotaFailure(record pluginapi.UsageRecord, now time.Time) (QuotaFailureEvent, bool)`
- detect only `Provider == "codex"`, `Failed == true`, and status code `429`
- parse JSON body for:
  - `error.type == "usage_limit_reached"`
  - `resets_at`
  - `resets_in_seconds`
- support reset fields both at top level and inside `error`
- ignore `rate_limit_error` without usage-limit markers
- if usage limit is detected but reset timing is missing, set `ResetAt = now.Add(2 * time.Minute)` and reason `usage_limit_reached_no_reset`

- [ ] **Step 4: Wire `usage.handle` in `main.go`**

In dispatch for `pluginabi.MethodUsageHandle`:

- Decode `pluginapi.UsageRecord`.
- If `globalState.Config().EnableUsageFeedback == false`, return empty OK response.
- Call `DetectQuotaFailure(record, time.Now())`.
- If detected, call `globalState.MarkAccountTemporaryExhausted(event.AuthID, event.ResetAt, event.Reason)`.
- Return empty OK response.

- [ ] **Step 5: Run tests**

Run:

```powershell
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add usage.go usage_test.go main.go
git commit -m "feat: react to codex quota failures"
```

## Task 6: Host Callback Refresh Service

**Files:**
- Create: `refresh.go`
- Create: `refresh_test.go`
- Modify: `main.go`

- [ ] **Step 1: Write failing refresh tests with fake host**

Create `refresh_test.go` with:

```go
package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type fakeHostClient struct {
	authList []pluginapi.HostAuthFileEntry
	authJSON map[string]json.RawMessage
	httpBody []byte
}

func (f fakeHostClient) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	return f.authList, nil
}

func (f fakeHostClient) GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	return pluginapi.HostAuthGetResponse{AuthIndex: authIndex, JSON: f.authJSON[authIndex]}, nil
}

func (f fakeHostClient) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if req.Headers.Get("Authorization") != "Bearer access-1" {
		return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte("bad auth")}, nil
	}
	if req.Headers.Get("Chatgpt-Account-Id") != "acct-1" {
		return pluginapi.HTTPResponse{StatusCode: 400, Body: []byte("bad account")}, nil
	}
	return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{}, Body: f.httpBody}, nil
}

func TestRefreshOnceLoadsCodexAuthAndQuota(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	host := fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex", Email: "a@example.com", Priority: 7,
		}},
		authJSON: map[string]json.RawMessage{
			"idx-1": json.RawMessage(`{"access_token":"access-1","id_token":"` + idToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	store := NewPluginState(DefaultConfig())
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := refresher.RefreshOnce(); err != nil {
		t.Fatalf("RefreshOnce returned error: %v", err)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	if snapshot.Accounts[0].ChatGPTAccountID != "acct-1" {
		t.Fatalf("account = %#v", snapshot.Accounts[0])
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```powershell
go test ./...
```

Expected: FAIL because refresh interfaces do not exist.

- [ ] **Step 3: Implement refresh abstractions**

Create `refresh.go` with:

- `type HostClient interface { ListAuths() ([]pluginapi.HostAuthFileEntry, error); GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error); Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error); Log(level, message string, fields map[string]any) }`
- `type ABIHostClient struct{}`
- `NewQuotaRefresher(host HostClient, state *PluginState, now func() time.Time) *QuotaRefresher`
- `RefreshOnce() error`
- `Start()`
- `Stop()`

`RefreshOnce` behavior:

- Call `host.auth.list`.
- Filter provider `codex`, not disabled, not unavailable, has auth index.
- For each auth, call `host.auth.get`.
- Extract credentials with `ExtractCodexCredentials`.
- Call `host.http.do` using `GET cfg.QuotaEndpoint` with:
  - `Authorization: Bearer <access_token>`
  - `Chatgpt-Account-Id: <chatgpt_account_id>`
  - `Content-Type: application/json`
  - `User-Agent: codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal`
- Parse response with `ParseCodexUsagePayload`.
- Upsert `AccountState`.
- Redact access tokens from all errors before storing or logging.

`ABIHostClient` must use the same envelope handling pattern as `host-callback-auth-files/go/main.go` and support:

- `pluginabi.MethodHostAuthList`
- `pluginabi.MethodHostAuthGet`
- `pluginabi.MethodHostHTTPDo`
- `pluginabi.MethodHostLog`

- [ ] **Step 4: Wire refresh lifecycle**

In `main.go`:

- Create global refresher after host API is stored in `cliproxy_plugin_init`.
- On `plugin.register` and `plugin.reconfigure`, call `globalRefresher.RefreshSoon()` or start the loop if not running.
- On `cliproxyPluginShutdown`, stop the refresher.

The first implementation can make `RefreshSoon()` run in a goroutine guarded by a mutex so repeated reconfigure calls do not start overlapping refreshes.

- [ ] **Step 5: Run tests**

Run:

```powershell
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add refresh.go refresh_test.go main.go
git commit -m "feat: refresh quota through host callbacks"
```

## Task 7: Management Status UI And Routes

**Files:**
- Create: `management.go`
- Create: `management_test.go`
- Modify: `main.go`

- [ ] **Step 1: Write failing management tests**

Create `management_test.go` with:

```go
package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementRegisterExposesStatusResourceAndRoutes(t *testing.T) {
	resp := RegisterManagement()
	if len(resp.Resources) != 1 || resp.Resources[0].Path != "/status" {
		t.Fatalf("resources = %#v", resp.Resources)
	}
	paths := map[string]string{}
	for _, route := range resp.Routes {
		paths[route.Method+" "+route.Path] = route.Path
	}
	for _, key := range []string{
		"GET /plugins/codex-quota-scheduler/status",
		"POST /plugins/codex-quota-scheduler/refresh",
		"GET /plugins/codex-quota-scheduler/annotations",
		"PUT /plugins/codex-quota-scheduler/annotations",
	} {
		if paths[key] == "" {
			t.Fatalf("missing route %s in %#v", key, paths)
		}
	}
}

func TestStatusJSONOrdersAccountsBySchedulerOrder(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(weeklyAccount("later", 5, now.Add(48*time.Hour), false))
	store.UpsertQuota(weeklyAccount("earlier", 5, now.Add(24*time.Hour), false))
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d", resp.StatusCode)
	}
	var body struct {
		Accounts []struct{ AuthID string `json:"auth_id"` } `json:"accounts"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if len(body.Accounts) != 2 || body.Accounts[0].AuthID != "earlier" {
		t.Fatalf("accounts = %#v", body.Accounts)
	}
}

func TestStatusHTMLRedactsSensitiveFields(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false))
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/status",
	}, now)
	html := string(resp.Body)
	if !strings.Contains(html, "codex-quota-scheduler") {
		t.Fatalf("html missing title: %s", html)
	}
	if strings.Contains(strings.ToLower(html), "access_token") || strings.Contains(strings.ToLower(html), "bearer ") {
		t.Fatalf("html leaks sensitive content: %s", html)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```powershell
go test ./...
```

Expected: FAIL because management handlers do not exist.

- [ ] **Step 3: Implement management registration and routing**

Create `management.go` with:

- `RegisterManagement() pluginapi.ManagementRegistrationResponse`
- `HandleManagementRequest(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse`
- `BuildStatusPayload(snapshot StateSnapshot, ordered []ScheduledAccount) StatusPayload`
- `RenderStatusHTML(payload StatusPayload) []byte`

Routes:

- Resource: `GET /status`
- Management: `GET /plugins/codex-quota-scheduler/status`
- Management: `POST /plugins/codex-quota-scheduler/refresh`
- Management: `GET /plugins/codex-quota-scheduler/annotations`
- Management: `PUT /plugins/codex-quota-scheduler/annotations`
- Management: `PATCH /plugins/codex-quota-scheduler/annotations/account`
- Management: `PATCH /plugins/codex-quota-scheduler/annotations/group`

`GET status` behavior:

- `?format=json` returns JSON.
- default returns HTML.
- account order must come from `BuildOrderedAccounts`.
- include `next_auth_id`, `monthly_mode`, `handle_enabled`, `last_selected`, `last_reason`.

`PUT annotations` behavior:

- Decode `AnnotationState`.
- Normalize and store it in `PluginState`.
- If `AnnotationStatePath` is non-empty, persist with `SaveAnnotations`.
- Return JSON status `{ "ok": true }`.

HTML constraints:

- No external scripts.
- No token, raw auth JSON, cookie, or Authorization header content.
- Use a compact table sorted by scheduler order.
- Include columns: rank, auth ID, alias, group, tags, CPA priority, family, availability, reset/expiry, cache age.

- [ ] **Step 4: Wire management dispatch in `main.go`**

For `management.register`, return `RegisterManagement()`.

For `management.handle`, decode `pluginapi.ManagementRequest`, call `HandleManagementRequest(globalState, req, time.Now())`, and return the response.

For `POST /refresh`, call `globalRefresher.RefreshSoon()` and return `202 Accepted`.

- [ ] **Step 5: Run tests**

Run:

```powershell
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add management.go management_test.go main.go
git commit -m "feat: expose scheduler status management UI"
```

## Task 8: Final Integration, Build, And Manual Verification Notes

**Files:**
- Modify: `README.md`
- Modify: `build.ps1`
- Create: `integration_test.go`

- [ ] **Step 1: Write integration tests for ABI dispatch**

Create `integration_test.go` with:

```go
package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHandleMethodRegisterEnvelope(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodPluginRegister, []byte(`{"config_yaml":"aGFuZGxlX2VuYWJsZWQ6IHRydWUK"}`))
	if err != nil {
		t.Fatalf("handleMethod returned error: %v", err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope = %#v", env)
	}
}

func TestHandleMethodSchedulerPickFallbackEnvelope(t *testing.T) {
	globalState = NewPluginState(DefaultConfig())
	rawReq, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "missing", Provider: "codex", Priority: 1}},
	})
	raw, err := handleMethod(pluginabi.MethodSchedulerPick, rawReq)
	if err != nil {
		t.Fatalf("handleMethod returned error: %v", err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope = %#v", env)
	}
	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal pick response: %v", err)
	}
	if !resp.Handled || resp.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("response = %#v", resp)
	}
}
```

- [ ] **Step 2: Run tests**

Run:

```powershell
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Run Windows dynamic library build**

Run:

```powershell
.\build.ps1
```

Expected:

- `go test ./...` passes.
- `dist/codex-quota-scheduler.dll` exists.
- `dist/codex-quota-scheduler.h` exists because `-buildmode=c-shared` emits a header.

- [ ] **Step 4: Update README with verification flow**

Add:

```markdown
## Manual Verification

1. Copy `dist/codex-quota-scheduler.dll` into CPA's plugin directory for the current platform.
2. Enable global plugins and this plugin in CPA config.
3. Start CPA and confirm `GET /v0/management/plugins` reports `registered: true` and `effective_enabled: true`.
4. Open `/v0/resource/plugins/codex-quota-scheduler/status`.
5. Confirm the first account in the status page matches the scheduler order:
   CPA priority descending, then monthly mode ordering inside the priority tier.
6. Send a Codex request and confirm the selected auth ID matches the status page's top selectable account.
7. Simulate or observe a 429 `usage_limit_reached` response and confirm the next scheduler pick avoids that account.
```

- [ ] **Step 5: Run final verification**

Run:

```powershell
go test ./...
.\build.ps1
git status --short
```

Expected:

- tests pass
- build succeeds
- only intentional source/docs changes are shown by git

- [ ] **Step 6: Commit**

```powershell
git add README.md build.ps1 integration_test.go
git commit -m "test: verify plugin dispatch and build"
```

## Implementation Notes

- Use `Handled=false` only for disabled plugin, non-Codex requests, or requests with no Codex candidates.
- For Codex requests where the plugin is enabled but cannot select from fresh quota state, use `DelegateBuiltin: fill-first` so fallback is explicit and deterministic.
- Do not use remaining percentage for sorting. Remaining percentage is only an availability signal.
- CPA candidate priority is the source of truth for scheduling tiers.
- Do not persist or render access tokens, id tokens, cookies, Authorization headers, or raw auth JSON.
- Keep host HTTP quota refresh out of `scheduler.pick`; scheduler picks must read cached state only.
- Keep annotation metadata display-only in v1.

## Final Verification Checklist

- `go test ./...`
- `.\build.ps1`
- Review `git diff --stat`
- Open the status resource in a local CPA instance when available
- Confirm no generated `.dll`, `.h`, `.test`, `dist/`, or state JSON files are committed
