package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementRegisterExposesStatusResourceAndRoutes(t *testing.T) {
	resp := RegisterManagement()
	resources := map[string]pluginapi.ResourceRoute{}
	for _, resource := range resp.Resources {
		resources[resource.Path] = resource
	}
	for _, path := range []string{"/status"} {
		if _, ok := resources[path]; !ok {
			t.Fatalf("missing resource %s in %#v", path, resp.Resources)
		}
	}
	if resources["/status"].Menu == "" {
		t.Fatalf("status resource Menu is empty: %#v", resources["/status"])
	}
	if len(resources) != len(resp.Resources) {
		t.Fatalf("resources = %#v", resp.Resources)
	}

	paths := map[string]string{}
	for _, route := range resp.Routes {
		paths[route.Method+" "+route.Path] = route.Path
		if route.Method == http.MethodGet && route.Path == "/plugins/codex-quota-scheduler/status" && route.Menu != "" {
			t.Fatalf("management status route Menu = %q, want empty", route.Menu)
		}
	}
	for _, key := range []string{
		"GET /plugins/codex-quota-scheduler/status",
		"GET /plugins/codex-quota-scheduler/settings",
		"PUT /plugins/codex-quota-scheduler/settings",
		"POST /plugins/codex-quota-scheduler/refresh",
		"POST /plugins/codex-quota-scheduler/refresh/account",
		"GET /plugins/codex-quota-scheduler/logs",
		"GET /plugins/codex-quota-scheduler/export",
		"POST /plugins/codex-quota-scheduler/import",
		"GET /plugins/codex-quota-scheduler/annotations",
		"PUT /plugins/codex-quota-scheduler/annotations",
		"PATCH /plugins/codex-quota-scheduler/annotations/account",
		"PATCH /plugins/codex-quota-scheduler/annotations/group",
	} {
		if paths[key] == "" {
			t.Fatalf("missing route %s in %#v", key, paths)
		}
	}
}

func TestManagementRoutesDispatchFullCPAPaths(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false))

	refreshes := 0
	refreshOne := ""
	previousRefreshSoon := managementRefreshSoon
	previousRefreshOneSoon := managementRefreshOneSoon
	managementRefreshSoon = func() { refreshes++ }
	managementRefreshOneSoon = func(authID string) { refreshOne = authID }
	t.Cleanup(func() {
		managementRefreshSoon = previousRefreshSoon
		managementRefreshOneSoon = previousRefreshOneSoon
	})

	tests := []struct {
		name   string
		method string
		path   string
		query  url.Values
		body   []byte
		want   int
	}{
		{
			name:   "management status",
			method: http.MethodGet,
			path:   "/v0/management/plugins/codex-quota-scheduler/status",
			query:  url.Values{"format": []string{"json"}},
			want:   http.StatusOK,
		},
		{
			name:   "resource status",
			method: http.MethodGet,
			path:   "/v0/resource/plugins/codex-quota-scheduler/status",
			want:   http.StatusOK,
		},
		{
			name:   "management refresh",
			method: http.MethodPost,
			path:   "/v0/management/plugins/codex-quota-scheduler/refresh",
			want:   http.StatusAccepted,
		},
		{
			name:   "management refresh one",
			method: http.MethodPost,
			path:   "/v0/management/plugins/codex-quota-scheduler/refresh/account",
			body:   []byte(`{"auth_id":"auth-1"}`),
			want:   http.StatusAccepted,
		},
		{
			name:   "resource mutation hidden",
			method: http.MethodGet,
			path:   "/v0/resource/plugins/codex-quota-scheduler/refresh",
			want:   http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
				Method: tt.method,
				Path:   tt.path,
				Query:  tt.query,
				Body:   tt.body,
			}, now)
			if resp.StatusCode != tt.want {
				t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, tt.want, resp.Body)
			}
		})
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	if refreshOne != "auth-1" {
		t.Fatalf("refreshOne = %q, want auth-1", refreshOne)
	}
}

func TestStatusJSONOrdersAccountsBySchedulerOrder(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	later := weeklyAccount("later", 5, now.Add(48*time.Hour), false)
	later.LastSuccessAt = now
	earlier := weeklyAccount("earlier", 5, now.Add(24*time.Hour), false)
	earlier.LastSuccessAt = now
	store.UpsertQuota(later)
	store.UpsertQuota(earlier)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	if strings.Contains(string(resp.Body), "action_token") {
		t.Fatalf("status JSON exposed action token: %s", resp.Body)
	}
	var body struct {
		Accounts []struct {
			AuthID string `json:"auth_id"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Accounts) != 2 || body.Accounts[0].AuthID != "earlier" {
		t.Fatalf("accounts = %#v, want earlier first", body.Accounts)
	}
}

func TestStatusJSONMovesUnavailableAccountsBehindAvailableAccounts(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	exhausted := weeklyAccount("exhausted", 5, now.Add(25*time.Hour), true)
	available := weeklyAccount("available", 5, now.Add(48*time.Hour), false)
	available.LastSuccessAt = now
	store.UpsertQuota(exhausted)
	store.UpsertQuota(available)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Accounts) != 2 {
		t.Fatalf("accounts len = %d, want 2; accounts=%#v", len(body.Accounts), body.Accounts)
	}
	if body.Accounts[0].AuthID != "available" || body.Accounts[1].AuthID != "exhausted" {
		t.Fatalf("accounts = %#v, want available then exhausted", body.Accounts)
	}
	if !body.Accounts[0].Available {
		t.Fatalf("available account = %#v, want available=true", body.Accounts[0])
	}
	if body.Accounts[1].Available || body.Accounts[1].UnavailableReason == "" {
		t.Fatalf("unavailable account = %#v, want available=false with reason", body.Accounts[1])
	}
}

func TestStatusJSONIncludesSchedulerSummary(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModePriority
	cfg.HandleEnabled = false
	store := NewPluginState(cfg)
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now
	store.UpsertQuota(account)
	store.RecordSelection("auth-1", "selected")

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)

	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if body.NextAuthID != "auth-1" || body.MonthlyMode != MonthlyModePriority || body.HandleEnabled || body.LastSelected != "auth-1" || body.LastReason != "selected" {
		t.Fatalf("payload summary = %#v", body)
	}
}

func TestStatusNextAuthIDDoesNotScanBelowActivePriority(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	highBlocked := weeklyAccount("high-blocked", 10, now.Add(24*time.Hour), false)
	highBlocked.Stale = true
	lowAvailable := weeklyAccount("low-available", 1, now.Add(time.Hour), false)
	lowAvailable.LastSuccessAt = now
	store.UpsertQuota(highBlocked)
	store.UpsertQuota(lowAvailable)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if body.NextAuthID != "" {
		t.Fatalf("NextAuthID = %q, want empty because highest CPA priority tier has no available account", body.NextAuthID)
	}
}

func TestStatusJSONIncludesEmptyLastSelectionFields(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	lastSelected, ok := body["last_selected"]
	if !ok || lastSelected != "" {
		t.Fatalf("last_selected = %#v, present=%t; body=%s", lastSelected, ok, resp.Body)
	}
	lastReason, ok := body["last_reason"]
	if !ok || lastReason != "" {
		t.Fatalf("last_reason = %#v, present=%t; body=%s", lastReason, ok, resp.Body)
	}
}

func TestStatusHTMLRedactsSensitiveFieldsAndEscapesUserFields(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: `<script>alert("x")</script>`, Tags: []string{"team"}, GroupID: "ops"},
		},
		Groups: map[string]GroupAnnotation{
			"ops": {Name: "Ops & Finance"},
		},
	})
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	store.UpsertQuota(account)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "GET", Path: "/status"}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	html := string(resp.Body)
	lower := strings.ToLower(html)
	if !strings.Contains(html, "codex-quota-scheduler") {
		t.Fatalf("html missing plugin id: %s", html)
	}
	for _, want := range []string{"Codex 额度调度器", "调度设置", "账号队列", "保存设置", "刷新额度", "账号卡片"} {
		if !strings.Contains(html, want) {
			t.Fatalf("html missing Chinese UI text %q: %s", want, html)
		}
	}
	if strings.Contains(html, "<table") {
		t.Fatalf("html still renders table layout: %s", html)
	}
	for _, forbidden := range []string{"access_token", "bearer ", "authorization", "cookie"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("html contains sensitive field %q: %s", forbidden, html)
		}
	}
	if strings.Contains(html, "<script>alert") || !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, "Ops &amp; Finance") {
		t.Fatalf("html did not escape user fields: %s", html)
	}
}

func TestStatusHTMLUsesResourceActionsModalProgressAndLogs(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now
	store.UpsertQuota(account)
	store.RecordLog("info", "scheduler.selected", "请求已由插件接管", map[string]any{"auth_id": "auth-1"}, now)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "GET", Path: "/status"}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	html := string(resp.Body)
	lower := strings.ToLower(html)
	for _, want := range []string{"quota-bar", "editDialog", "logList", "openEdit", "exportLogs", "codex-quota-scheduler-logs.json", "maxLogEntries", "logRetention", "refreshOneQuota", "schedulePageRefresh", "RESOURCE_ENDPOINT", "/v0/resource/plugins/codex-quota-scheduler/status", "requestPlugin(action,options)", "localeSelect", "TRANSLATIONS", "codex-quota-scheduler-locale-v1", "Scheduler Settings", "Account Queue", "INLINE_TRANSLATIONS", "Reset credits", "Refresh Quota"} {
		if !strings.Contains(html, want) {
			t.Fatalf("html missing marker %q: %s", want, html)
		}
	}
	for _, forbidden := range []string{"managementKey", "missing management key", "details class=\"editor\"", "action_token", "MANAGEMENT_BASE", "/v0/management/plugins/codex-quota-scheduler"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("html still contains removed marker %q: %s", forbidden, html)
		}
	}
	for _, forbidden := range []string{"bearer ", "authorization", "cookie"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("html contains sensitive field %q: %s", forbidden, html)
		}
	}
}

func TestStatusJSONIncludesCircuitStateAndResetCredits(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now
	count := 2
	account.Quota.ResetCreditsAvailableCount = &count
	total := 3
	account.Quota.ResetCreditsTotalEarnedCount = &total
	account.Quota.ResetCredits = []ResetCredit{
		{ExpiresAt: now.Add(30 * 24 * time.Hour)},
	}
	account.Circuit = CircuitBreakerState{
		State:        CircuitStateOpen,
		FailureCount: 3,
		OpenedAt:     now.Add(-time.Minute),
		NextProbeAt:  now.Add(9 * time.Minute),
		Reason:       usageLimitReachedReason,
	}
	store.UpsertQuota(account)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(body.Accounts))
	}
	accountBody := body.Accounts[0]
	if accountBody.Circuit.State != CircuitStateOpen || accountBody.Circuit.Label != "熔断" || accountBody.Circuit.FailureCount != 3 || accountBody.UnavailableReason != "circuit_open" {
		t.Fatalf("circuit = %#v unavailable=%q", accountBody.Circuit, accountBody.UnavailableReason)
	}
	if accountBody.ResetCreditsAvailableCount == nil || *accountBody.ResetCreditsAvailableCount != 2 || len(accountBody.ResetCredits) != 1 {
		t.Fatalf("reset credits = count %#v list %#v", accountBody.ResetCreditsAvailableCount, accountBody.ResetCredits)
	}
	if accountBody.ResetCreditsTotalEarnedCount == nil || *accountBody.ResetCreditsTotalEarnedCount != 3 {
		t.Fatalf("total earned reset credits = %#v", accountBody.ResetCreditsTotalEarnedCount)
	}
}

func TestSettingsEndpointUpdatesConfigAndPersistsDefaultState(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/plugins/codex-quota-scheduler/settings",
		Body:   []byte(`{"handle_enabled":false,"monthly_mode":"priority","quota_refresh_interval":"45s","stale_after":"15m","enable_usage_feedback":false,"max_refresh_concurrency":2,"max_log_entries":25,"log_retention":"3h"}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}

	cfg := store.Config()
	if cfg.HandleEnabled || cfg.MonthlyMode != MonthlyModePriority || cfg.QuotaRefreshInterval != 45*time.Second || cfg.StaleAfter != 15*time.Minute || cfg.EnableUsageFeedback || cfg.MaxRefreshConcurrency != 2 || cfg.MaxLogEntries != 25 || cfg.LogRetention != 3*time.Hour {
		t.Fatalf("config = %#v", cfg)
	}
	disk, err := LoadPluginDiskState(defaultStatePath())
	if err != nil {
		t.Fatalf("LoadPluginDiskState returned error: %v", err)
	}
	if disk.Config.MonthlyMode != MonthlyModePriority || disk.Config.HandleEnabled {
		t.Fatalf("persisted config = %#v", disk.Config)
	}
}

func TestStatusJSONIncludesQuotaWindowsForProgressBars(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	fiveHourUsed := 100.0
	weeklyUsed := 40.0
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), true)
	account.Quota.FiveHour.UsedPercent = &fiveHourUsed
	account.Quota.FiveHour.ResetAt = now.Add(time.Hour)
	account.Quota.LongWindow.UsedPercent = &weeklyUsed
	store.UpsertQuota(account)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/plugins/codex-quota-scheduler/status",
		Query:  url.Values{"format": []string{"json"}},
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body StatusPayload
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(body.Accounts))
	}
	accountBody := body.Accounts[0]
	if accountBody.FiveHour.UsedPercent != 100 || accountBody.FiveHour.RemainingPercent != 0 || accountBody.FiveHour.DisplayText != "已用完" || !accountBody.FiveHour.Exhausted {
		t.Fatalf("five hour window = %#v, want exhausted with zero remaining", accountBody.FiveHour)
	}
	if accountBody.LongWindow.UsedPercent != 40 || accountBody.LongWindow.RemainingPercent != 60 || accountBody.LongWindow.DisplayText != "剩余 60%" || accountBody.LongWindow.Label == "" {
		t.Fatalf("long window = %#v, want weekly remaining label and percent", accountBody.LongWindow)
	}
}

func TestStatusWindowPrefersRealUsagePercentOverExhaustedFlag(t *testing.T) {
	used := 40.0
	status := statusWindow(&QuotaWindow{
		Kind:        WindowWeekly,
		UsedPercent: &used,
		Exhausted:   true,
	}, "周额度")
	if status.Exhausted {
		t.Fatalf("Exhausted = true, want false when used_percent shows quota remains: %#v", status)
	}
	if status.RemainingPercent != 60 || status.DisplayText != "剩余 60%" {
		t.Fatalf("window = %#v, want remaining quota from real used_percent", status)
	}
}

func TestStatusHTMLShowsRemainingQuotaLocalResetTimesAndCompactMetadata(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "主力号", Notes: "账号备注内容", Tags: []string{"team", "paid"}, GroupID: "ops"},
		},
		Groups: map[string]GroupAnnotation{
			"ops": {Name: "运营组", Notes: "分组备注内容"},
		},
	})
	usedFive := 25.0
	usedLong := 40.0
	account := weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false)
	account.LastSuccessAt = now
	account.Quota.FiveHour.UsedPercent = &usedFive
	account.Quota.FiveHour.ResetAt = now.Add(3 * time.Hour)
	account.Quota.LongWindow.UsedPercent = &usedLong
	store.UpsertQuota(account)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: "GET", Path: "/status"}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	html := string(resp.Body)
	for _, want := range []string{"剩余 75%", "剩余 60%", "5 小时重置", "长额度重置", "localTime", "data-time=\"2026-06-21T12:00:00Z\"", "主力号", "运营组", "账号备注内容", "分组备注内容"} {
		if !strings.Contains(html, want) {
			t.Fatalf("html missing %q: %s", want, html)
		}
	}
}

func TestManagementSettingsEndpointDoesNotRequireManagementKey(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/codex-quota-scheduler/settings",
		Body:   []byte(`{"handle_enabled":false,"monthly_mode":"priority","quota_refresh_interval":"45m","stale_after":"6h","enable_usage_feedback":false,"max_refresh_concurrency":2,"max_log_entries":30,"log_retention":"4h"}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	cfg := store.Config()
	if cfg.HandleEnabled || cfg.MonthlyMode != MonthlyModePriority || cfg.QuotaRefreshInterval != 45*time.Minute || cfg.StaleAfter != 6*time.Hour || cfg.EnableUsageFeedback || cfg.MaxRefreshConcurrency != 2 || cfg.MaxLogEntries != 30 || cfg.LogRetention != 4*time.Hour {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestResourceRoutesServePluginPageEndpoints(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/codex-quota-scheduler/status",
		Query: url.Values{
			"action":  []string{"settings"},
			"payload": []string{`{"handle_enabled":false,"monthly_mode":"priority","quota_refresh_interval":"45m","stale_after":"6h","enable_usage_feedback":false,"max_refresh_concurrency":2,"max_log_entries":30,"log_retention":"4h"}`},
		},
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settings StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	if store.Config().HandleEnabled || store.Config().MonthlyMode != MonthlyModePriority {
		t.Fatalf("resource settings did not update config: %#v", store.Config())
	}
	for _, action := range []string{
		"logs",
		"export",
	} {
		resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   "/v0/resource/plugins/codex-quota-scheduler/status",
			Query:  url.Values{"action": []string{action}},
		}, time.Now())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s StatusCode = %d, want %d; body=%s", action, resp.StatusCode, http.StatusOK, resp.Body)
		}
	}
}

func TestResourceStatusRendersUsablePluginPage(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "secret alias", Notes: "private note"},
		},
	})
	store.UpsertQuota(weeklyAccount("auth-1", 5, now.Add(24*time.Hour), false))
	store.RecordLog("info", "scheduler.selected", "private log", map[string]any{"auth_id": "auth-1"}, now)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/codex-quota-scheduler/status",
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	body := string(resp.Body)
	for _, forbidden := range []string{"window.location.replace", "http-equiv=\"refresh\"", "/v0/management/plugins/codex-quota-scheduler/status"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("resource status still depends on management redirect marker %q: %s", forbidden, body)
		}
	}
	for _, want := range []string{"secret alias", "private note", "private log", "logList", "RESOURCE_ENDPOINT", "/v0/resource/plugins/codex-quota-scheduler/status", "requestPlugin(action,options)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("resource status missing usable plugin page marker %q: %s", want, body)
		}
	}
}

func TestManagementAccountEndpointDoesNotRequireManagementKey(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPatch,
		Path:   "/v0/management/plugins/codex-quota-scheduler/annotations/account",
		Body:   []byte(`{"auth_id":"auth-1","alias":"工作账号","group_id":"team-a","tags":["team","paid"],"notes":"常用"}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	got := store.Annotations().Accounts["auth:auth-1"]
	if got.Alias != "工作账号" || got.GroupID != "team-a" || len(got.Tags) != 2 || got.Notes != "常用" {
		t.Fatalf("account annotation = %#v", got)
	}
}

func TestManagementAccountEndpointClearsTags(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "A", Tags: []string{"wrong", "old"}},
		},
	})

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPatch,
		Path:   "/v0/management/plugins/codex-quota-scheduler/annotations/account",
		Body:   []byte(`{"auth_id":"auth-1","tags":[]}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	if tags := store.Annotations().Accounts["auth:auth-1"].Tags; len(tags) != 0 {
		t.Fatalf("tags = %#v, want cleared", tags)
	}
}

func TestManagementGroupEndpointAllowsClearingFields(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Groups: map[string]GroupAnnotation{
			"1": {Name: "group1", Notes: "keep", Color: "#00f"},
		},
	})

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodPatch,
		Path:   "/v0/management/plugins/codex-quota-scheduler/annotations/group",
		Body:   []byte(`{"id":"1","name":"","notes":"","color":""}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	got := store.Annotations().Groups["1"]
	if got.Name != "" || got.Notes != "" || got.Color != "" {
		t.Fatalf("group = %#v, want clearable fields emptied", got)
	}
}

func TestResourceExportImportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModePriority
	store := NewPluginState(cfg)
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "A", Tags: []string{"team"}, GroupID: "1"},
		},
		Groups: map[string]GroupAnnotation{
			"1": {Name: "group1"},
		},
	})

	exportResp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/codex-quota-scheduler/export",
	}, time.Now())
	if exportResp.StatusCode != http.StatusOK {
		t.Fatalf("export StatusCode = %d, want %d; body=%s", exportResp.StatusCode, http.StatusOK, exportResp.Body)
	}

	imported := NewPluginState(DefaultConfig())
	importResp := HandleManagementRequest(imported, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/codex-quota-scheduler/import",
		Body:   exportResp.Body,
	}, time.Now())
	if importResp.StatusCode != http.StatusOK {
		t.Fatalf("import StatusCode = %d, want %d; body=%s", importResp.StatusCode, http.StatusOK, importResp.Body)
	}
	if imported.Config().MonthlyMode != MonthlyModePriority {
		t.Fatalf("imported config = %#v", imported.Config())
	}
	if got := imported.Annotations().Groups["1"].Name; got != "group1" {
		t.Fatalf("imported group name = %q, want group1", got)
	}
	if got := imported.Annotations().Accounts["auth:auth-1"].Alias; got != "A" {
		t.Fatalf("imported account alias = %q, want A", got)
	}
}

func TestLogsEndpointReturnsSchedulerDecision(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.RecordLog("info", "scheduler.selected", "请求已由插件接管", map[string]any{"auth_id": "auth-1"}, now)

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/codex-quota-scheduler/logs",
	}, now)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	var body struct {
		Logs []LogEntry `json:"logs"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v; body=%s", err, resp.Body)
	}
	if len(body.Logs) != 1 || body.Logs[0].Event != "scheduler.selected" {
		t.Fatalf("logs = %#v", body.Logs)
	}
}

func TestAnnotationsEndpointsNormalizePatchAndPersist(t *testing.T) {
	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:keep": {Alias: "Keep", Tags: []string{"old"}},
		},
		Groups: map[string]GroupAnnotation{
			"group-1": {Name: "Existing"},
		},
	})

	resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "PATCH",
		Path:   "/plugins/codex-quota-scheduler/annotations/account",
		Body:   []byte(`{"key":"auth:new","alias":" New ","tags":["alpha","alpha"," "],"group_id":"group-1"}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}

	state := store.Annotations()
	if state.Accounts["auth:keep"].Alias != "Keep" {
		t.Fatalf("unrelated account annotation was not preserved: %#v", state.Accounts)
	}
	got := state.Accounts["auth:new"]
	if got.Alias != " New " || len(got.Tags) != 1 || got.Tags[0] != "alpha" || got.GroupID != "group-1" {
		t.Fatalf("patched account annotation = %#v", got)
	}
	raw, err := os.ReadFile(defaultStatePath())
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if !strings.Contains(string(raw), `"auth:new"`) {
		t.Fatalf("persisted annotations missing patched key: %s", raw)
	}

	resp = HandleManagementRequest(store, pluginapi.ManagementRequest{
		Method: "PATCH",
		Path:   "/plugins/codex-quota-scheduler/annotations/group",
		Body:   []byte(`{"id":"group-2","annotation":{"name":"Blue","tags":["x","x"],"color":"#00f"}}`),
	}, time.Now())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
	}
	state = store.Annotations()
	if state.Groups["group-1"].Name != "Existing" || state.Groups["group-2"].Name != "Blue" || len(state.Groups["group-2"].Tags) != 1 {
		t.Fatalf("group annotations = %#v", state.Groups)
	}
}

func TestAnnotationsPersistenceFailureDoesNotMutateMemory(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   []byte
		check  func(t *testing.T, state AnnotationState)
	}{
		{
			name:   "put",
			method: http.MethodPut,
			path:   "/plugins/codex-quota-scheduler/annotations",
			body:   []byte(`{"accounts":{"auth:new":{"alias":"New"}}}`),
			check: func(t *testing.T, state AnnotationState) {
				if _, ok := state.Accounts["auth:new"]; ok {
					t.Fatalf("failed PUT mutated annotations: %#v", state.Accounts)
				}
			},
		},
		{
			name:   "patch account",
			method: http.MethodPatch,
			path:   "/plugins/codex-quota-scheduler/annotations/account",
			body:   []byte(`{"key":"auth:new","alias":"New"}`),
			check: func(t *testing.T, state AnnotationState) {
				if _, ok := state.Accounts["auth:new"]; ok {
					t.Fatalf("failed account PATCH mutated annotations: %#v", state.Accounts)
				}
			},
		},
		{
			name:   "patch group",
			method: http.MethodPatch,
			path:   "/plugins/codex-quota-scheduler/annotations/group",
			body:   []byte(`{"id":"group-new","name":"New"}`),
			check: func(t *testing.T, state AnnotationState) {
				if _, ok := state.Groups["group-new"]; ok {
					t.Fatalf("failed group PATCH mutated annotations: %#v", state.Groups)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previousDefaultStatePath := defaultStatePath
			defaultStatePath = func() string { return filepath.Join(t.TempDir(), "state.json") + string(rune(0)) }
			t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

			store := NewPluginState(DefaultConfig())
			store.SetAnnotations(AnnotationState{
				Accounts: map[string]AccountAnnotation{
					"auth:keep": {Alias: "Keep"},
				},
				Groups: map[string]GroupAnnotation{
					"group-keep": {Name: "Keep"},
				},
			})

			resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
				Method: tt.method,
				Path:   tt.path,
				Body:   tt.body,
			}, time.Now())
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusInternalServerError, resp.Body)
			}
			state := store.Annotations()
			if state.Accounts["auth:keep"].Alias != "Keep" || state.Groups["group-keep"].Name != "Keep" {
				t.Fatalf("existing annotations changed after failed persistence: %#v", state)
			}
			tt.check(t, state)
		})
	}
}
