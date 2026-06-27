package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const managementBasePath = "/plugins/" + PluginID

var managementRefreshSoon = func() {}
var managementRefreshOneSoon = func(authID string) {}

type StatusPayload struct {
	PluginID      string          `json:"plugin_id"`
	GeneratedAt   time.Time       `json:"generated_at"`
	NextAuthID    string          `json:"next_auth_id"`
	MonthlyMode   MonthlyMode     `json:"monthly_mode"`
	HandleEnabled bool            `json:"handle_enabled"`
	LastSelected  string          `json:"last_selected"`
	LastReason    string          `json:"last_reason"`
	Settings      SettingsPayload `json:"settings"`
	Accounts      []StatusAccount `json:"accounts"`
	Groups        []StatusGroup   `json:"groups,omitempty"`
	Logs          []LogEntry      `json:"logs"`
}

type SettingsPayload struct {
	HandleEnabled                   bool        `json:"handle_enabled"`
	MonthlyMode                     MonthlyMode `json:"monthly_mode"`
	QuotaRefreshInterval            string      `json:"quota_refresh_interval"`
	StaleAfter                      string      `json:"stale_after"`
	EnableUsageFeedback             bool        `json:"enable_usage_feedback"`
	MaxRefreshConcurrency           int         `json:"max_refresh_concurrency"`
	QuotaEndpoint                   string      `json:"quota_endpoint"`
	RefreshActiveWindow             string      `json:"refresh_active_window"`
	RefreshAfterResetDelay          string      `json:"refresh_after_reset_delay"`
	RefreshRetryDelays              string      `json:"refresh_retry_delays"`
	RefreshOnStartup                bool        `json:"refresh_on_startup"`
	CircuitFailureThreshold         int         `json:"circuit_failure_threshold"`
	CircuitOpenDuration             string      `json:"circuit_open_duration"`
	CircuitHalfOpenSuccessThreshold int         `json:"circuit_half_open_success_threshold"`
	MaxLogEntries                   int         `json:"max_log_entries"`
	LogRetention                    string      `json:"log_retention"`
}

type StatusAccount struct {
	Rank                         int           `json:"rank"`
	AuthID                       string        `json:"auth_id"`
	Alias                        string        `json:"alias,omitempty"`
	Notes                        string        `json:"notes,omitempty"`
	GroupID                      string        `json:"group_id,omitempty"`
	Group                        string        `json:"group,omitempty"`
	GroupNotes                   string        `json:"group_notes,omitempty"`
	Tags                         []string      `json:"tags,omitempty"`
	CPAPriority                  int           `json:"cpa_priority"`
	Family                       AccountFamily `json:"family"`
	QueueStatus                  QueueStatus   `json:"queue_status"`
	Available                    bool          `json:"available"`
	UnavailableReason            string        `json:"unavailable_reason,omitempty"`
	ResetExpiry                  time.Time     `json:"reset_expiry,omitempty"`
	ResetExpiryText              string        `json:"reset_expiry_text,omitempty"`
	CacheAge                     string        `json:"cache_age,omitempty"`
	CacheAgeSeconds              int64         `json:"cache_age_seconds,omitempty"`
	LastError                    string        `json:"last_error,omitempty"`
	RefreshDueReason             string        `json:"refresh_due_reason,omitempty"`
	NextRetryText                string        `json:"next_retry_text,omitempty"`
	AuthFailure                  bool          `json:"auth_failure,omitempty"`
	FiveHour                     StatusWindow  `json:"five_hour"`
	LongWindow                   StatusWindow  `json:"long_window"`
	Circuit                      StatusCircuit `json:"circuit"`
	ResetCreditsAvailableCount   *int          `json:"reset_credits_available_count,omitempty"`
	ResetCreditsTotalEarnedCount *int          `json:"reset_credits_total_earned_count,omitempty"`
	ResetCredits                 []ResetCredit `json:"reset_credits,omitempty"`
}

type StatusCircuit struct {
	State         CircuitState `json:"state"`
	Label         string       `json:"label"`
	FailureCount  int          `json:"failure_count"`
	SuccessCount  int          `json:"success_count"`
	NextProbeAt   time.Time    `json:"next_probe_at,omitempty"`
	NextProbeText string       `json:"next_probe_text,omitempty"`
	Reason        string       `json:"reason,omitempty"`
}

type StatusGroup struct {
	ID    string   `json:"id"`
	Name  string   `json:"name,omitempty"`
	Notes string   `json:"notes,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Color string   `json:"color,omitempty"`
}

type StatusWindow struct {
	Kind             WindowKind `json:"kind,omitempty"`
	Label            string     `json:"label"`
	UsedPercent      float64    `json:"used_percent"`
	RemainingPercent float64    `json:"remaining_percent"`
	DisplayText      string     `json:"display_text"`
	Exhausted        bool       `json:"exhausted"`
	ResetAt          time.Time  `json:"reset_at,omitempty"`
	ResetText        string     `json:"reset_text,omitempty"`
	Missing          bool       `json:"missing"`
}

func RegisterManagement() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/status",
				Menu:        "Codex 调度器",
				Description: "Open scheduler quota status.",
			},
		},
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: managementBasePath + "/status", Description: "Scheduler quota status."},
			{Method: http.MethodGet, Path: managementBasePath + "/settings", Description: "Read scheduler settings."},
			{Method: http.MethodPut, Path: managementBasePath + "/settings", Description: "Update scheduler settings."},
			{Method: http.MethodPost, Path: managementBasePath + "/refresh", Description: "Refresh quota status soon."},
			{Method: http.MethodPost, Path: managementBasePath + "/refresh/account", Description: "Refresh one account quota soon."},
			{Method: http.MethodGet, Path: managementBasePath + "/logs", Description: "Read scheduler logs."},
			{Method: http.MethodGet, Path: managementBasePath + "/export", Description: "Export scheduler configuration."},
			{Method: http.MethodPost, Path: managementBasePath + "/import", Description: "Import scheduler configuration."},
			{Method: http.MethodGet, Path: managementBasePath + "/annotations", Description: "Read quota annotations."},
			{Method: http.MethodPut, Path: managementBasePath + "/annotations", Description: "Replace quota annotations."},
			{Method: http.MethodPatch, Path: managementBasePath + "/annotations/account", Description: "Update one account annotation."},
			{Method: http.MethodPatch, Path: managementBasePath + "/annotations/group", Description: "Update one group annotation."},
		},
	}
}

func HandleManagementRequest(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	if store == nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": "plugin state unavailable"})
	}
	if now.IsZero() {
		now = time.Now()
	}

	method := strings.ToUpper(req.Method)
	path := normalizeManagementPath(req.Path)
	if isResourcePath(req.Path) && !resourceRouteAllowed(method, path) {
		return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
	switch {
	case method == http.MethodGet && path == "/status":
		return handleStatusRequest(store, req, now)
	case method == http.MethodGet && path == "/settings":
		return jsonManagementResponse(http.StatusOK, SettingsFromConfig(store.Config()))
	case method == http.MethodPut && path == "/settings":
		return handlePutSettings(store, req, now)
	case method == http.MethodPost && path == "/refresh":
		triggerRefreshSoon()
		store.RecordLog("info", "ui.refresh_requested", "页面请求刷新额度", nil, now)
		return jsonManagementResponse(http.StatusAccepted, map[string]bool{"ok": true})
	case method == http.MethodPost && path == "/refresh/account":
		return handleRefreshAccountRequest(store, req, now)
	case method == http.MethodGet && path == "/logs":
		return jsonManagementResponse(http.StatusOK, map[string]any{"logs": store.Snapshot(now).Logs})
	case method == http.MethodGet && path == "/export":
		return handleExportState(store, now)
	case method == http.MethodPost && path == "/import":
		return handleImportState(store, req.Body, now)
	case method == http.MethodGet && path == "/annotations":
		return jsonManagementResponse(http.StatusOK, store.Annotations())
	case method == http.MethodPut && path == "/annotations":
		return handlePutAnnotations(store, req)
	case method == http.MethodPatch && path == "/annotations/account":
		return handlePatchAccountAnnotation(store, req, now)
	case method == http.MethodPatch && path == "/annotations/group":
		return handlePatchGroupAnnotation(store, req, now)
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func isResourcePath(path string) bool {
	return path == "/v0/resource"+managementBasePath || strings.HasPrefix(path, "/v0/resource"+managementBasePath+"/")
}

func resourceRouteAllowed(method, path string) bool {
	if method == http.MethodGet {
		switch path {
		case "/status":
			return true
		default:
			return false
		}
	}
	return false
}

func SettingsFromConfig(cfg Config) SettingsPayload {
	return SettingsPayload{
		HandleEnabled:                   cfg.HandleEnabled,
		MonthlyMode:                     cfg.MonthlyMode,
		QuotaRefreshInterval:            cfg.QuotaRefreshInterval.String(),
		StaleAfter:                      cfg.StaleAfter.String(),
		EnableUsageFeedback:             cfg.EnableUsageFeedback,
		MaxRefreshConcurrency:           cfg.MaxRefreshConcurrency,
		QuotaEndpoint:                   cfg.QuotaEndpoint,
		RefreshActiveWindow:             cfg.RefreshActiveWindow.String(),
		RefreshAfterResetDelay:          cfg.RefreshAfterResetDelay.String(),
		RefreshRetryDelays:              formatDurationList(cfg.RefreshRetryDelays),
		RefreshOnStartup:                cfg.RefreshOnStartup,
		CircuitFailureThreshold:         cfg.CircuitFailureThreshold,
		CircuitOpenDuration:             cfg.CircuitOpenDuration.String(),
		CircuitHalfOpenSuccessThreshold: cfg.CircuitHalfOpenSuccessThreshold,
		MaxLogEntries:                   cfg.MaxLogEntries,
		LogRetention:                    cfg.LogRetention.String(),
	}
}

func ConfigFromSettings(base Config, payload SettingsPayload) (Config, error) {
	cfg := NormalizeConfig(base)
	if payload.MonthlyMode != "" {
		cfg.MonthlyMode = payload.MonthlyMode
	}
	if cfg.MonthlyMode != MonthlyModePriority && cfg.MonthlyMode != MonthlyModeExpiryOrder {
		return Config{}, jsonError("monthly_mode must be expiry_order or priority")
	}
	if payload.QuotaRefreshInterval != "" {
		d, err := time.ParseDuration(payload.QuotaRefreshInterval)
		if err != nil || d <= 0 {
			return Config{}, jsonError("quota_refresh_interval must be a positive duration")
		}
		cfg.QuotaRefreshInterval = d
	}
	if payload.StaleAfter != "" {
		d, err := time.ParseDuration(payload.StaleAfter)
		if err != nil || d <= 0 {
			return Config{}, jsonError("stale_after must be a positive duration")
		}
		cfg.StaleAfter = d
	}
	cfg.HandleEnabled = payload.HandleEnabled
	cfg.EnableUsageFeedback = payload.EnableUsageFeedback
	if payload.MaxRefreshConcurrency <= 0 {
		return Config{}, jsonError("max_refresh_concurrency must be positive")
	}
	cfg.MaxRefreshConcurrency = payload.MaxRefreshConcurrency
	if strings.TrimSpace(payload.QuotaEndpoint) != "" {
		endpoint, err := validateQuotaEndpoint(payload.QuotaEndpoint)
		if err != nil {
			return Config{}, err
		}
		cfg.QuotaEndpoint = endpoint
	}
	if payload.RefreshActiveWindow != "" {
		d, err := time.ParseDuration(payload.RefreshActiveWindow)
		if err != nil || d <= 0 {
			return Config{}, jsonError("refresh_active_window must be a positive duration")
		}
		cfg.RefreshActiveWindow = d
	}
	if payload.RefreshAfterResetDelay != "" {
		d, err := time.ParseDuration(payload.RefreshAfterResetDelay)
		if err != nil || d <= 0 {
			return Config{}, jsonError("refresh_after_reset_delay must be a positive duration")
		}
		cfg.RefreshAfterResetDelay = d
	}
	if payload.RefreshRetryDelays != "" {
		delays, err := parseDurationList(payload.RefreshRetryDelays)
		if err != nil {
			return Config{}, jsonError("refresh_retry_delays must be positive comma-separated durations")
		}
		cfg.RefreshRetryDelays = delays
	}
	cfg.RefreshOnStartup = payload.RefreshOnStartup
	if payload.CircuitFailureThreshold > 0 {
		cfg.CircuitFailureThreshold = payload.CircuitFailureThreshold
	}
	if payload.CircuitOpenDuration != "" {
		d, err := time.ParseDuration(payload.CircuitOpenDuration)
		if err != nil || d <= 0 {
			return Config{}, jsonError("circuit_open_duration must be a positive duration")
		}
		cfg.CircuitOpenDuration = d
	}
	if payload.CircuitHalfOpenSuccessThreshold > 0 {
		cfg.CircuitHalfOpenSuccessThreshold = payload.CircuitHalfOpenSuccessThreshold
	}
	if payload.MaxLogEntries > 0 {
		cfg.MaxLogEntries = payload.MaxLogEntries
	}
	if payload.LogRetention != "" {
		d, err := time.ParseDuration(payload.LogRetention)
		if err != nil || d <= 0 {
			return Config{}, jsonError("log_retention must be a positive duration")
		}
		cfg.LogRetention = d
	}
	return NormalizeConfig(cfg), nil
}

func formatDurationList(values []time.Duration) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value > 0 {
			parts = append(parts, value.String())
		}
	}
	return strings.Join(parts, ",")
}

type jsonError string

func (e jsonError) Error() string { return string(e) }

func handlePutSettings(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	var payload SettingsPayload
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	resp := saveSettingsPayload(store, payload)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		store.RecordLog("info", "ui.settings_saved", "页面保存调度设置", nil, now)
	}
	return resp
}

func saveSettingsPayload(store *PluginState, payload SettingsPayload) pluginapi.ManagementResponse {
	cfg, err := ConfigFromSettings(store.Config(), payload)
	if err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	store.ReplaceConfig(cfg)
	currentConfig.Store(cfg)
	if err := SavePluginDiskState(defaultStatePath(), diskStateFromStore(store)); err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return jsonManagementResponse(http.StatusOK, SettingsFromConfig(cfg))
}

type refreshAccountPayload struct {
	AuthID string `json:"auth_id"`
}

func handleRefreshAccountRequest(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	authID := strings.TrimSpace(req.Query.Get("auth_id"))
	if authID == "" && len(req.Body) > 0 {
		var payload refreshAccountPayload
		if err := json.Unmarshal(req.Body, &payload); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		authID = strings.TrimSpace(payload.AuthID)
	}
	if authID == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "auth_id is required"})
	}
	triggerRefreshOneSoon(authID)
	store.RecordLog("info", "ui.refresh_one_requested", "页面请求刷新单个账号额度", map[string]any{"auth_id": authID}, now)
	return jsonManagementResponse(http.StatusAccepted, map[string]bool{"ok": true})
}

func handleExportState(store *PluginState, now time.Time) pluginapi.ManagementResponse {
	state := diskStateFromStore(store)
	store.RecordLog("info", "ui.config_exported", "页面导出插件配置", nil, now)
	return jsonManagementResponse(http.StatusOK, normalizePluginDiskState(state))
}

func handleImportState(store *PluginState, body []byte, now time.Time) pluginapi.ManagementResponse {
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "data is required"})
	}
	var state PluginDiskState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if _, err := validateQuotaEndpoint(state.Config.QuotaEndpoint); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	state = normalizePluginDiskState(state)
	if err := SavePluginDiskState(defaultStatePath(), state); err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	store.ReplaceConfig(state.Config)
	currentConfig.Store(state.Config)
	store.SetAnnotations(AnnotationState{Accounts: state.Accounts, Groups: state.Groups})
	store.RecordLog("info", "ui.config_imported", "页面导入插件配置", nil, now)
	return jsonManagementResponse(http.StatusOK, map[string]bool{"ok": true})
}

func BuildStatusPayload(snapshot StateSnapshot, ordered []ScheduledAccount) StatusPayload {
	accountsByAuthID := make(map[string]AccountState, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if account.AuthID != "" {
			accountsByAuthID[account.AuthID] = account
		}
	}

	payload := StatusPayload{
		PluginID:      PluginID,
		GeneratedAt:   snapshot.Now,
		MonthlyMode:   snapshot.Config.MonthlyMode,
		HandleEnabled: snapshot.Config.HandleEnabled,
		LastSelected:  snapshot.LastSelected,
		LastReason:    snapshot.LastReason,
		Settings:      SettingsFromConfig(snapshot.Config),
		Accounts:      make([]StatusAccount, 0, len(ordered)),
		Groups:        make([]StatusGroup, 0, len(snapshot.Annotations.Groups)),
		Logs:          cloneLogs(snapshot.Logs),
	}
	payload.NextAuthID = nextStatusAuthID(ordered)
	for id, group := range snapshot.Annotations.Groups {
		payload.Groups = append(payload.Groups, StatusGroup{
			ID:    id,
			Name:  group.Name,
			Notes: group.Notes,
			Tags:  cloneStringSlice(group.Tags),
			Color: group.Color,
		})
	}
	for i, scheduled := range ordered {
		account := accountsByAuthID[scheduled.AuthID]
		groupID := scheduled.Annotation.GroupID
		groupName := groupID
		if group, ok := snapshot.Annotations.Groups[groupID]; ok && group.Name != "" {
			groupName = group.Name
		}
		groupNotes := ""
		if group, ok := snapshot.Annotations.Groups[groupID]; ok {
			groupNotes = group.Notes
		}

		status := StatusAccount{
			Rank:              i + 1,
			AuthID:            scheduled.AuthID,
			Alias:             scheduled.Annotation.Alias,
			Notes:             scheduled.Annotation.Notes,
			GroupID:           groupID,
			Group:             groupName,
			GroupNotes:        groupNotes,
			Tags:              cloneStringSlice(scheduled.Annotation.Tags),
			CPAPriority:       scheduled.Priority,
			Family:            scheduled.Family,
			QueueStatus:       scheduled.QueueStatus,
			Available:         scheduled.Available,
			UnavailableReason: scheduled.UnavailableReason,
			ResetExpiry:       scheduled.SortTime,
			ResetExpiryText:   formatTime(scheduled.SortTime),
			LastError:         account.LastError,
			NextRetryText:     formatTime(account.Refresh.NextRetryAt),
			AuthFailure:       account.Refresh.AuthFailure,
			FiveHour:          statusWindow(account.Quota.FiveHour, "5 小时额度"),
			LongWindow:        statusWindow(account.Quota.LongWindow, longWindowLabelCN(account.Family)),
		}
		_, status.RefreshDueReason = accountRefreshDue(account, snapshot.Config, snapshot.Now)
		status.Circuit = statusCircuit(account.Circuit, snapshot.Now)
		status.ResetCreditsAvailableCount = cloneIntPtr(account.Quota.ResetCreditsAvailableCount)
		status.ResetCreditsTotalEarnedCount = cloneIntPtr(account.Quota.ResetCreditsTotalEarnedCount)
		status.ResetCredits = append([]ResetCredit(nil), account.Quota.ResetCredits...)
		if !account.LastSuccessAt.IsZero() {
			age := snapshot.Now.Sub(account.LastSuccessAt)
			if age < 0 {
				age = 0
			}
			status.CacheAge = age.Round(time.Second).String()
			status.CacheAgeSeconds = int64(age.Seconds())
		}
		payload.Accounts = append(payload.Accounts, status)
	}
	return payload
}

func nextStatusAuthID(ordered []ScheduledAccount) string {
	if len(ordered) == 0 {
		return ""
	}
	activePriority := ordered[0].Priority
	for _, scheduled := range ordered {
		if scheduled.Priority != activePriority {
			break
		}
		if scheduled.Available {
			return scheduled.AuthID
		}
	}
	return ""
}

func statusWindow(window *QuotaWindow, label string) StatusWindow {
	if window == nil {
		return StatusWindow{Label: label, DisplayText: "暂无数据", Missing: true}
	}
	used := 0.0
	hasUsagePercent := window.UsedPercent != nil
	if window.UsedPercent != nil {
		used = *window.UsedPercent
	}
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	remaining := 100 - used
	if remaining < 0 {
		remaining = 0
	}
	exhausted := window.Exhausted
	if hasUsagePercent {
		exhausted = used >= 100
	}
	if exhausted {
		remaining = 0
	}
	displayText := fmt.Sprintf("剩余 %.0f%%", remaining)
	if exhausted {
		displayText = "已用完"
	}
	return StatusWindow{
		Kind:             window.Kind,
		Label:            label,
		UsedPercent:      used,
		RemainingPercent: remaining,
		DisplayText:      displayText,
		Exhausted:        exhausted,
		ResetAt:          window.ResetAt,
		ResetText:        formatTime(window.ResetAt),
	}
}

func statusCircuit(circuit CircuitBreakerState, now time.Time) StatusCircuit {
	circuit = effectiveCircuitState(circuit, now)
	label := "全开"
	switch circuit.EffectiveState {
	case CircuitStateOpen:
		label = "熔断"
	case CircuitStateHalfOpen:
		label = "半开"
	}
	return StatusCircuit{
		State:         circuit.EffectiveState,
		Label:         label,
		FailureCount:  circuit.FailureCount,
		SuccessCount:  circuit.SuccessCount,
		NextProbeAt:   circuit.NextProbeAt,
		NextProbeText: formatTime(circuit.NextProbeAt),
		Reason:        circuit.Reason,
	}
}

func cloneIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func longWindowLabel(family AccountFamily) string {
	if family == AccountFamilyMonthly {
		return "月额度"
	}
	return "周额度"
}

func longWindowLabelCN(family AccountFamily) string {
	if family == AccountFamilyMonthly {
		return "月额度"
	}
	return "周额度"
}

func RenderStatusHTML(payload StatusPayload) []byte {
	var buf bytes.Buffer
	if err := statusTemplateV2.Execute(&buf, payload); err != nil {
		return []byte("<!doctype html><html><body>status unavailable</body></html>")
	}
	return buf.Bytes()
}

func handleStatusRequest(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	snapshot := store.Snapshot(now)
	ordered := BuildOrderedAccounts(syntheticStatusRequest(snapshot), snapshot, now)
	payload := BuildStatusPayload(snapshot, ordered)
	if req.Query.Get("format") == "json" && !isResourcePath(req.Path) {
		return jsonManagementResponse(http.StatusOK, payload)
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       RenderStatusHTML(payload),
	}
}

func syntheticStatusRequest(snapshot StateSnapshot) pluginapi.SchedulerPickRequest {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if account.AuthID == "" {
			continue
		}
		provider := account.Provider
		if provider == "" {
			provider = "codex"
		}
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{
			ID:       account.AuthID,
			Provider: provider,
			Priority: account.Priority,
			Status:   "active",
		})
	}
	return pluginapi.SchedulerPickRequest{
		Provider:   "codex",
		Providers:  []string{"codex"},
		Candidates: candidates,
	}
}

func handlePutAnnotations(store *PluginState, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var state AnnotationState
	if err := json.Unmarshal(req.Body, &state); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	state = NormalizeAnnotationState(state)
	if err := persistAnnotationState(store, state); err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	store.SetAnnotations(state)
	return jsonManagementResponse(http.StatusOK, map[string]bool{"ok": true})
}

func handlePatchAccountAnnotation(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	var patch annotationPatch
	if err := json.Unmarshal(req.Body, &patch); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	resp := applyAccountAnnotationPatch(store, patch)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		store.RecordLog("info", "ui.account_saved", "页面保存账号卡片", map[string]any{"auth_id": patch.AuthID, "key": patch.Key}, now)
	}
	return resp
}

func applyAccountAnnotationPatch(store *PluginState, patch annotationPatch) pluginapi.ManagementResponse {
	key := patch.accountKey()
	if key == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "annotation key is required"})
	}

	state := store.Annotations()
	annotation := state.Accounts[key]
	if len(patch.Annotation) > 0 {
		if err := json.Unmarshal(patch.Annotation, &annotation); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}
	if patch.Alias != nil {
		annotation.Alias = *patch.Alias
	}
	if patch.Notes != nil {
		annotation.Notes = *patch.Notes
	}
	if patch.Tags != nil {
		annotation.Tags = patch.Tags
	}
	if patch.GroupID != nil {
		annotation.GroupID = *patch.GroupID
	}
	state.Accounts[key] = annotation
	state = NormalizeAnnotationState(state)
	if err := persistAnnotationState(store, state); err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	store.SetAnnotations(state)
	return jsonManagementResponse(http.StatusOK, map[string]bool{"ok": true})
}

func handlePatchGroupAnnotation(store *PluginState, req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	var patch annotationPatch
	if err := json.Unmarshal(req.Body, &patch); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	resp := applyGroupAnnotationPatch(store, patch)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		store.RecordLog("info", "ui.group_saved", "页面保存账号分组", map[string]any{"group_id": patch.ID, "key": patch.Key}, now)
	}
	return resp
}

func applyGroupAnnotationPatch(store *PluginState, patch annotationPatch) pluginapi.ManagementResponse {
	key := patch.groupKey()
	if key == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "group key is required"})
	}

	state := store.Annotations()
	annotation := state.Groups[key]
	if len(patch.Annotation) > 0 {
		if err := json.Unmarshal(patch.Annotation, &annotation); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}
	if patch.Name != nil {
		annotation.Name = *patch.Name
	}
	if patch.Notes != nil {
		annotation.Notes = *patch.Notes
	}
	if patch.Tags != nil {
		annotation.Tags = patch.Tags
	}
	if patch.Color != nil {
		annotation.Color = *patch.Color
	}
	state.Groups[key] = annotation
	state = NormalizeAnnotationState(state)
	if err := persistAnnotationState(store, state); err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	store.SetAnnotations(state)
	return jsonManagementResponse(http.StatusOK, map[string]bool{"ok": true})
}

type annotationPatch struct {
	Key        string          `json:"key"`
	ID         string          `json:"id"`
	AuthID     string          `json:"auth_id"`
	Alias      *string         `json:"alias"`
	Name       *string         `json:"name"`
	Notes      *string         `json:"notes"`
	Tags       []string        `json:"tags"`
	GroupID    *string         `json:"group_id"`
	Color      *string         `json:"color"`
	Annotation json.RawMessage `json:"annotation"`
}

func (p annotationPatch) accountKey() string {
	if p.Key != "" {
		return p.Key
	}
	if p.ID != "" {
		return p.ID
	}
	if p.AuthID != "" {
		return "auth:" + p.AuthID
	}
	return ""
}

func (p annotationPatch) groupKey() string {
	if p.Key != "" {
		return p.Key
	}
	return p.ID
}

func persistAnnotations(store *PluginState) error {
	return persistAnnotationState(store, store.Annotations())
}

func persistAnnotationState(store *PluginState, state AnnotationState) error {
	if store == nil {
		return nil
	}
	disk := diskStateFromStore(store)
	disk.Accounts = state.Accounts
	disk.Groups = state.Groups
	return SavePluginDiskState(defaultStatePath(), disk)
}

func triggerRefreshSoon() {
	managementRefreshSoon()
}

func triggerRefreshOneSoon(authID string) {
	managementRefreshOneSoon(authID)
}

func normalizeManagementPath(path string) string {
	for _, prefix := range []string{
		"/v0/management" + managementBasePath,
		"/v0/resource" + managementBasePath,
		managementBasePath,
	} {
		if path == prefix {
			return "/"
		}
		if strings.HasPrefix(path, prefix+"/") {
			path = strings.TrimPrefix(path, prefix)
			break
		}
	}
	if path == "/v0/resource"+managementBasePath+"/status" {
		return "/status"
	}
	if path == "" {
		return "/"
	}
	return path
}

func jsonManagementResponse(status int, body any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(body)
	if err != nil {
		status = http.StatusInternalServerError
		raw = []byte(`{"error":"failed to encode response"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

var statusTemplateV2 = template.Must(template.New("status-v2").Funcs(template.FuncMap{
	"join": strings.Join,
	"json": jsonForTemplate,
}).Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Codex 额度调度器</title>
<style>
*{box-sizing:border-box}body{font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;color:#1f2937;background:#f6f7f9}button,input,select,textarea{font:inherit}button{border:0;border-radius:7px;padding:9px 12px;background:#2563eb;color:#fff;cursor:pointer;font-weight:650}button.secondary{background:#eef2ff;color:#1e40af}button.ghost{background:#f3f4f6;color:#374151}button:disabled{opacity:.55;cursor:not-allowed}code{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:12px}.shell{display:grid;grid-template-columns:310px minmax(0,1fr);min-height:100vh}.sidebar{background:#fff;border-right:1px solid #e5e7eb;padding:18px;position:sticky;top:0;height:100vh;overflow:auto}.main{padding:20px 22px 32px;overflow:auto}.brand{display:grid;gap:5px;margin-bottom:18px}.brand h1{font-size:20px;line-height:1.2;margin:0;color:#111827}.brand p{font-size:12px;line-height:1.45;color:#6b7280;margin:0}.section{border-top:1px solid #eef0f3;padding-top:16px;margin-top:16px}.section h2{font-size:14px;margin:0 0 12px;color:#111827}.field{display:grid;gap:6px;margin-bottom:12px}.field span{font-size:12px;color:#4b5563;font-weight:650}.field input,.field select,.field textarea{width:100%;border:1px solid #d1d5db;border-radius:7px;background:#fff;color:#111827;padding:8px 10px}.field textarea{min-height:84px;resize:vertical}.toggle{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:12px}.toggle span{font-size:13px;font-weight:650}.actions{display:flex;gap:8px;flex-wrap:wrap}.notice{margin-top:12px;border-radius:7px;padding:10px 11px;background:#ecfdf5;color:#065f46;font-size:12px;line-height:1.45}.notice.error{background:#fef2f2;color:#991b1b}.toolbar{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:18px}.toolbar h2{font-size:22px;margin:0;color:#111827}.toolbar p{font-size:13px;color:#6b7280;margin:5px 0 0}.metrics{display:flex;gap:8px;flex-wrap:wrap;justify-content:flex-end}.metric{border:1px solid #e5e7eb;background:#fff;border-radius:7px;padding:8px 10px;font-size:12px;color:#374151}.queue{display:grid;grid-template-columns:repeat(auto-fill,minmax(310px,1fr));gap:12px}.card{background:#fff;border:1px solid #e5e7eb;border-radius:8px;padding:14px;display:grid;gap:12px;box-shadow:0 1px 2px rgba(15,23,42,.04)}.card.next{border-color:#2563eb;box-shadow:0 0 0 1px rgba(37,99,235,.18),0 8px 24px rgba(37,99,235,.08)}.cardTop{display:flex;align-items:flex-start;justify-content:space-between;gap:10px}.rank{display:inline-flex;align-items:center;justify-content:center;min-width:30px;height:30px;border-radius:7px;background:#111827;color:#fff;font-weight:750;font-size:13px}.identity{min-width:0;display:grid;gap:5px}.titleLine{display:flex;align-items:center;gap:7px;flex-wrap:wrap}.title{font-weight:750;color:#111827;overflow-wrap:anywhere}.groupPill{border-radius:999px;background:#f0fdf4;color:#166534;padding:3px 7px;font-size:11px}.sub{font-size:12px;color:#6b7280;overflow-wrap:anywhere}.badges{display:flex;gap:6px;flex-wrap:wrap}.badge{border-radius:999px;background:#f3f4f6;color:#374151;padding:4px 8px;font-size:12px}.badge.ok{background:#dcfce7;color:#166534}.badge.no{background:#fee2e2;color:#991b1b}.badge.next{background:#dbeafe;color:#1d4ed8}.kv{display:grid;grid-template-columns:88px minmax(0,1fr);gap:6px 10px;font-size:12px}.kv span:nth-child(odd){color:#6b7280}.kv span:nth-child(even){color:#111827;overflow-wrap:anywhere}.chips{display:flex;gap:6px;flex-wrap:wrap;min-height:24px}.chip{border-radius:999px;background:#eef2ff;color:#3730a3;padding:4px 8px;font-size:12px}.quotaList{display:grid;gap:10px}.quota-row{display:grid;gap:5px}.quota-head{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:8px;font-size:12px;color:#374151}.quota-title{font-weight:650;color:#111827}.quota-reset{grid-column:1/-1;color:#6b7280}.localTime{color:#374151}.quota-bar{height:8px;border-radius:999px;background:#e5e7eb;overflow:hidden}.quota-fill{height:100%;border-radius:999px;background:#2f7d5f}.quota-fill.warn{background:#b7791f}.quota-fill.danger{background:#dc2626}.metaLine{display:flex;gap:6px;flex-wrap:wrap}.noteBlock{font-size:12px;line-height:1.45;color:#4b5563;background:#f9fafb;border-radius:7px;padding:8px 9px;display:grid;gap:3px}.cardActions{display:flex;justify-content:flex-end}.empty{background:#fff;border:1px dashed #d1d5db;border-radius:8px;padding:28px;text-align:center;color:#6b7280}.logs{margin-top:20px;background:#fff;border:1px solid #e5e7eb;border-radius:8px;padding:14px}.logsHeader{display:flex;justify-content:space-between;align-items:center;gap:12px;margin-bottom:10px}.logsHeader h2{font-size:16px;margin:0}.logList{display:grid;gap:8px;max-height:260px;overflow:auto}.logItem{border-left:3px solid #d1d5db;padding:7px 9px;background:#f9fafb;border-radius:0 7px 7px 0}.logItem.info{border-left-color:#2563eb}.logItem.warn{border-left-color:#b7791f}.logItem.error{border-left-color:#dc2626}.logMeta{font-size:11px;color:#6b7280;margin-bottom:3px}.logMsg{font-size:12px;color:#111827;line-height:1.45}dialog{border:0;border-radius:8px;padding:0;width:min(560px,calc(100vw - 28px));box-shadow:0 24px 64px rgba(15,23,42,.28)}dialog::backdrop{background:rgba(15,23,42,.38)}.dialogBody{padding:18px}.dialogHead{display:flex;justify-content:space-between;gap:12px;align-items:flex-start;margin-bottom:14px}.dialogHead h2{font-size:18px;margin:0;color:#111827}.dialogHead p{font-size:12px;color:#6b7280;margin:4px 0 0}.dialogGrid{display:grid;grid-template-columns:1fr 1fr;gap:10px}.dialogGrid .wide{grid-column:1/-1}.dialogActions{display:flex;justify-content:flex-end;gap:8px;margin-top:14px}@media(max-width:860px){.shell{grid-template-columns:1fr}.sidebar{height:auto;position:relative;border-right:0;border-bottom:1px solid #e5e7eb}.toolbar{display:grid}.metrics{justify-content:flex-start}.dialogGrid{grid-template-columns:1fr}}
</style>
</head>
<body>
<div class="shell">
<aside class="sidebar">
<div class="brand"><h1 data-i18n="app.title">Codex 额度调度器</h1><p data-i18n="app.subtitle">优化版 Fill First。配置、别名、分组、标签和备注由插件内部状态文件保存。</p></div>
<label class="field"><span data-i18n="app.language">界面语言</span><select id="localeSelect"><option value="zh-CN">中文</option><option value="en">English</option></select></label>
<label class="field"><span data-i18n="connection.managementKey">CPA 管理密钥</span><input id="managementKey" type="password" autocomplete="off" spellcheck="false"></label>
<div class="section"><h2 data-i18n="settings.title">调度设置</h2>
<label class="toggle"><span data-i18n="settings.handleEnabled">启用调度接管</span><input id="handleEnabled" type="checkbox"></label>
<label class="toggle"><span data-i18n="settings.usageFeedback">失败反馈标记额度耗尽</span><input id="usageFeedback" type="checkbox"></label>
<label class="field"><span data-i18n="settings.monthlyMode">Monthly 模式</span><select id="monthlyMode"><option value="expiry_order" data-i18n="settings.expiryOrder">按到期时间排序</option><option value="priority" data-i18n="settings.monthlyPriority">优先使用 Monthly</option></select></label>
<label class="field"><span data-i18n="settings.refreshInterval">额度刷新间隔</span><input id="refreshInterval" spellcheck="false"></label>
<label class="field"><span data-i18n="settings.staleAfter">缓存过期判定</span><input id="staleAfter" spellcheck="false"></label>
<label class="field"><span data-i18n="settings.maxConcurrency">最大并发刷新</span><input id="maxConcurrency" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="settings.circuitFailureThreshold">熔断失败阈值</span><input id="circuitFailureThreshold" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="settings.circuitOpenDuration">熔断等待时间</span><input id="circuitOpenDuration" spellcheck="false"></label>
<label class="field"><span data-i18n="settings.circuitHalfOpenSuccessThreshold">半开恢复成功次数</span><input id="circuitHalfOpenSuccessThreshold" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="settings.maxLogEntries">最大日志条数</span><input id="maxLogEntries" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="settings.logRetention">日志保留时间</span><input id="logRetention" spellcheck="false"></label>
<div class="actions"><button id="saveSettings" type="button" data-i18n="actions.saveSettings">保存设置</button><button id="refreshQuota" type="button" class="secondary" data-i18n="actions.refreshQuota">刷新额度</button><button id="exportConfig" type="button" class="ghost" data-i18n="actions.exportConfig">导出配置</button><button id="importConfig" type="button" class="ghost" data-i18n="actions.importConfig">导入配置</button><input id="importFile" type="file" accept="application/json,.json" hidden></div>
<div id="notice" class="notice" hidden></div>
</div>
</aside>
<main class="main">
<div class="toolbar"><div><h2 data-i18n="queue.title">账号队列</h2><p data-i18n="queue.description">账号卡片按当前调度优先级排序。第一个可用账号就是下一次 Codex 请求会优先选择的账号。</p></div><div class="metrics"><span class="metric"><span data-i18n="metrics.nextAccount">下一账号</span>：<code>{{if .NextAuthID}}{{.NextAuthID}}{{else}}暂无{{end}}</code></span><span class="metric">Monthly：{{if eq .MonthlyMode "priority"}}优先使用{{else}}按到期时间{{end}}</span><span class="metric"><span data-i18n="metrics.scheduler">调度</span>：{{if .HandleEnabled}}已启用{{else}}已关闭{{end}}</span><span class="metric"><span data-i18n="metrics.lastSelected">最近选择</span>：<code>{{if .LastSelected}}{{.LastSelected}}{{else}}暂无{{end}}</code></span></div></div>
<section class="queue" aria-label="账号卡片">{{range .Accounts}}<article class="card {{if and $.NextAuthID (eq $.NextAuthID .AuthID)}}next{{end}}" data-auth-id="{{.AuthID}}">
<div class="cardTop"><div class="identity"><div class="titleLine"><span class="title">{{if .Alias}}{{.Alias}}{{else}}{{.AuthID}}{{end}}</span>{{if .Group}}<span class="groupPill">{{.Group}}</span>{{end}}</div><div class="sub"><code>{{.AuthID}}</code></div>{{if .Tags}}<div class="metaLine">{{range .Tags}}<span class="chip">{{.}}</span>{{end}}</div>{{end}}</div><span class="rank">#{{.Rank}}</span></div>
<div class="badges">{{if and $.NextAuthID (eq $.NextAuthID .AuthID)}}<span class="badge next">下一优先</span>{{end}}{{if .Available}}<span class="badge ok">可用</span>{{else}}<span class="badge no">{{.UnavailableReason}}</span>{{end}}<span class="badge">{{if eq .Family "weekly"}}Weekly{{else if eq .Family "monthly"}}Monthly{{else}}未知类型{{end}}</span><span class="badge">CPA 优先级 {{.CPAPriority}}</span><span class="badge">熔断：{{.Circuit.Label}}</span></div>
<div class="quotaList">{{if not .FiveHour.Missing}}<div class="quota-row"><div class="quota-head"><span class="quota-title">{{.FiveHour.Label}}</span><span>{{.FiveHour.DisplayText}}</span><span class="quota-reset">5 小时重置：<span class="localTime" data-time="{{.FiveHour.ResetText}}">{{.FiveHour.ResetText}}</span></span></div><div class="quota-bar"><div class="quota-fill {{if .FiveHour.Exhausted}}danger{{else if le .FiveHour.RemainingPercent 20.0}}warn{{end}}" style="width:{{printf "%.0f" .FiveHour.RemainingPercent}}%"></div></div></div>{{end}}<div class="quota-row"><div class="quota-head"><span class="quota-title">{{.LongWindow.Label}}</span><span>{{.LongWindow.DisplayText}}</span>{{if not .LongWindow.Missing}}<span class="quota-reset">长额度重置：<span class="localTime" data-time="{{.LongWindow.ResetText}}">{{.LongWindow.ResetText}}</span></span>{{end}}</div><div class="quota-bar"><div class="quota-fill {{if .LongWindow.Exhausted}}danger{{else if le .LongWindow.RemainingPercent 20.0}}warn{{end}}" style="width:{{printf "%.0f" .LongWindow.RemainingPercent}}%"></div></div></div></div>
<div class="kv"><span>缓存时间</span><span>{{if .CacheAge}}{{.CacheAge}}{{else}}暂无{{end}}</span><span>熔断计数</span><span>失败 {{.Circuit.FailureCount}} / 成功 {{.Circuit.SuccessCount}}{{if .Circuit.NextProbeText}}，半开 <span class="localTime" data-time="{{.Circuit.NextProbeText}}">{{.Circuit.NextProbeText}}</span>{{end}}</span><span>主动重置</span><span>{{if .ResetCreditsAvailableCount}}{{.ResetCreditsAvailableCount}} 次{{else}}暂无{{end}}{{if .ResetCreditsTotalEarnedCount}} / 累计 {{.ResetCreditsTotalEarnedCount}} 次{{end}}{{range .ResetCredits}}；{{if .Status}}{{.Status}} {{end}}有效期 <span class="localTime" data-time="{{.ExpiresAt}}">{{.ExpiresAt}}</span>{{end}}</span></div>
{{if or .Notes .GroupNotes}}<div class="noteBlock">{{if .Notes}}<div>账号备注：{{.Notes}}</div>{{end}}{{if .GroupNotes}}<div>分组备注：{{.GroupNotes}}</div>{{end}}</div>{{end}}
<div class="cardActions"><button type="button" class="ghost refreshOne" data-auth-id="{{.AuthID}}">刷新额度</button><button type="button" class="secondary openEdit" data-auth-id="{{.AuthID}}">编辑</button></div>
</article>{{else}}<div class="empty">暂无账号数据。等待额度刷新后，这里会显示账号卡片。</div>{{end}}</section>
<section class="logs"><div class="logsHeader"><h2 data-i18n="logs.title">调度日志</h2><div class="actions"><button id="refreshLogs" type="button" class="ghost" data-i18n="actions.refreshLogs">刷新日志</button><button id="exportLogs" type="button" class="ghost" data-i18n="actions.exportLogs">导出日志</button></div></div><div id="logList" class="logList"></div></section>
</main>
</div>
<dialog id="editDialog"><form method="dialog" class="dialogBody"><div class="dialogHead"><div><h2 data-i18n="edit.title">编辑账号</h2><p id="editAuthID"></p></div><button type="button" id="closeDialog" class="ghost" data-i18n="actions.close">关闭</button></div><div class="dialogGrid"><label class="field"><span data-i18n="edit.alias">别名</span><input id="editAlias"></label><label class="field"><span data-i18n="edit.groupID">分组 ID</span><input id="editGroupID" placeholder="team-a"></label><label class="field"><span data-i18n="edit.groupName">分组名称</span><input id="editGroupName"></label><label class="field"><span data-i18n="edit.tags">标签</span><input id="editTags" placeholder="team, paid"></label><label class="field wide"><span data-i18n="edit.notes">账号备注</span><textarea id="editNotes"></textarea></label><label class="field wide"><span data-i18n="edit.groupNotes">分组备注</span><textarea id="editGroupNotes"></textarea></label></div><div class="dialogActions"><button type="button" id="saveAccount" class="secondary" data-i18n="actions.saveAccount">保存账号</button><button type="button" id="cancelEdit" class="ghost" data-i18n="actions.cancel">取消</button></div></form></dialog>
<script>
const STATUS={{json .}};
const MANAGEMENT_BASE='/v0/management/plugins/codex-quota-scheduler';
const LOCALE_STORAGE_KEY='codex-quota-scheduler-locale-v1';
const TRANSLATIONS={
  en:{
    'app.title':'Codex Quota Scheduler','app.subtitle':'Optimized Fill First scheduling. Configuration, aliases, groups, tags, and notes are saved in the plugin state file.','app.language':'Language','connection.managementKey':'CPA management key',
    'settings.title':'Scheduler Settings','settings.handleEnabled':'Enable scheduler takeover','settings.usageFeedback':'Mark quota exhausted from failure feedback','settings.monthlyMode':'Monthly mode','settings.expiryOrder':'Sort by expiry time','settings.monthlyPriority':'Prefer Monthly','settings.refreshInterval':'Quota refresh interval','settings.staleAfter':'Stale cache threshold','settings.maxConcurrency':'Max refresh concurrency','settings.circuitFailureThreshold':'Circuit failure threshold','settings.circuitOpenDuration':'Circuit open duration','settings.circuitHalfOpenSuccessThreshold':'Half-open recovery successes','settings.maxLogEntries':'Max log entries','settings.logRetention':'Log retention',
    'actions.saveSettings':'Save Settings','actions.refreshQuota':'Refresh Quota','actions.exportConfig':'Export Config','actions.importConfig':'Import Config','actions.refreshLogs':'Refresh Logs','actions.exportLogs':'Export Logs','actions.close':'Close','actions.saveAccount':'Save Account','actions.cancel':'Cancel',
    'queue.title':'Account Queue','queue.description':'Account cards are sorted by the current scheduler priority. The first available account is preferred for the next Codex request.','metrics.nextAccount':'Next account','metrics.scheduler':'Scheduler','metrics.lastSelected':'Last selected',
    'logs.title':'Scheduler Logs','logs.empty':'No logs yet. Send a request or refresh quota manually to show records here.',
    'edit.title':'Edit Account','edit.alias':'Alias','edit.groupID':'Group ID','edit.groupName':'Group name','edit.tags':'Tags','edit.notes':'Account notes','edit.groupNotes':'Group notes',
    'notice.settingsSaved':'Settings saved. The page will refresh shortly.','notice.refreshRequested':'Background quota refresh requested. The page will refresh shortly.','notice.accountSaved':'Account card saved. The page will refresh shortly.','notice.refreshOneRequested':'Quota refresh requested for this account. The page will refresh shortly.','notice.configExported':'Configuration exported.','notice.logsExported':'Logs exported.','notice.configImported':'Configuration imported. The page will refresh shortly.','error.requestFailed':'Request failed: {status}','error.managementKeyRequired':'CPA management key is required',
    'log.ui.refresh_requested':'UI requested quota refresh','log.ui.settings_saved':'UI saved scheduler settings','log.ui.refresh_one_requested':'UI requested one account quota refresh','log.ui.config_exported':'UI exported plugin configuration','log.ui.config_imported':'UI imported plugin configuration','log.ui.account_saved':'UI saved account card','log.ui.group_saved':'UI saved account group','log.scheduler.selected':'Request handled by plugin'
  },
  'zh-CN':{
    'app.title':'Codex 额度调度器','app.subtitle':'优化版 Fill First。配置、别名、分组、标签和备注由插件内部状态文件保存。','app.language':'界面语言','connection.managementKey':'CPA 管理密钥',
    'settings.title':'调度设置','settings.handleEnabled':'启用调度接管','settings.usageFeedback':'失败反馈标记额度耗尽','settings.monthlyMode':'Monthly 模式','settings.expiryOrder':'按到期时间排序','settings.monthlyPriority':'优先使用 Monthly','settings.refreshInterval':'额度刷新间隔','settings.staleAfter':'缓存过期判定','settings.maxConcurrency':'最大并发刷新','settings.circuitFailureThreshold':'熔断失败阈值','settings.circuitOpenDuration':'熔断等待时间','settings.circuitHalfOpenSuccessThreshold':'半开恢复成功次数','settings.maxLogEntries':'最大日志条数','settings.logRetention':'日志保留时间',
    'actions.saveSettings':'保存设置','actions.refreshQuota':'刷新额度','actions.exportConfig':'导出配置','actions.importConfig':'导入配置','actions.refreshLogs':'刷新日志','actions.exportLogs':'导出日志','actions.close':'关闭','actions.saveAccount':'保存账号','actions.cancel':'取消',
    'queue.title':'账号队列','queue.description':'账号卡片按当前调度优先级排序。第一个可用账号就是下一次 Codex 请求会优先选择的账号。','metrics.nextAccount':'下一账号','metrics.scheduler':'调度','metrics.lastSelected':'最近选择',
    'logs.title':'调度日志','logs.empty':'暂无日志。发起请求或手动刷新额度后，这里会显示记录。',
    'edit.title':'编辑账号','edit.alias':'别名','edit.groupID':'分组 ID','edit.groupName':'分组名称','edit.tags':'标签','edit.notes':'账号备注','edit.groupNotes':'分组备注',
    'notice.settingsSaved':'设置已保存，页面即将自动刷新。','notice.refreshRequested':'已请求后台刷新额度，页面稍后自动刷新。','notice.accountSaved':'账号卡片已保存，页面即将自动刷新。','notice.refreshOneRequested':'已请求刷新该账号额度，页面稍后自动刷新。','notice.configExported':'配置已导出。','notice.logsExported':'日志已导出。','notice.configImported':'配置已导入，页面即将自动刷新。','error.requestFailed':'请求失败：{status}','error.managementKeyRequired':'需要填写 CPA 管理密钥'
  }
};
const notice=document.getElementById('notice');
const editDialog=document.getElementById('editDialog');
const localeSelect=document.getElementById('localeSelect');
const accountsByID=new Map((STATUS.accounts||[]).map((account)=>[account.auth_id,account]));
const groupsByID=new Map();
for(const group of STATUS.groups||[]){if(group.id){groupsByID.set(group.id,{name:group.name||'',notes:group.notes||''})}}
for(const account of STATUS.accounts||[]){if(account.group_id){groupsByID.set(account.group_id,{name:account.group||'',notes:account.group_notes||''})}}
let editingAuthID='';
let currentLocale=detectLocale();
const INLINE_TRANSLATIONS=[
  ['下一优先','Next preferred'],['可用','Available'],['未知类型','unknown type'],['CPA 优先级','CPA priority'],['熔断：','Circuit: '],['熔断','Circuit'],['全开','closed'],['半开','half-open'],
  ['按到期时间','by expiry time'],['优先使用','prefer Monthly'],['已启用','enabled'],['已关闭','disabled'],['暂无','None'],
  ['5 小时额度','5-hour quota'],['周额度','Weekly quota'],['月额度','Monthly quota'],['剩余 ','Remaining '],['5 小时重置：','5-hour reset: '],['长额度重置：','Long quota reset: '],
  ['缓存时间','Cache age'],['熔断计数','Circuit count'],['失败','Failures'],['成功','Successes'],['主动重置','Reset credits'],[' 次',' times'],['累计','total'],['有效期','expires'],
  ['账号备注：','Account notes: '],['分组备注：','Group notes: '],['刷新额度','Refresh Quota'],['编辑','Edit'],['Monthly：','Monthly: '],['调度：','Scheduler: '],['下一账号：','Next account: '],['最近选择：','Last selected: ']
];
function normalizeLocale(raw){return String(raw||'').toLowerCase().startsWith('zh')?'zh-CN':'en'}
function detectLocale(){try{const saved=window.localStorage.getItem(LOCALE_STORAGE_KEY);if(saved)return normalizeLocale(saved)}catch(error){}const languages=navigator.languages&&navigator.languages.length?navigator.languages:[navigator.language];for(const language of languages){if(String(language||'').toLowerCase().startsWith('zh'))return'zh-CN'}return'en'}
function t(key,params){const dictionary=TRANSLATIONS[currentLocale]||TRANSLATIONS.en;let message=dictionary[key]||TRANSLATIONS.en[key]||key;for(const name of Object.keys(params||{})){message=message.split('{'+name+'}').join(String(params[name]))}return message}
function translateInlineText(raw){let text=String(raw||'');if(currentLocale!=='en')return text;for(const pair of INLINE_TRANSLATIONS){text=text.split(pair[0]).join(pair[1])}return text}
function applyInlineTranslations(){const nodes=document.querySelectorAll('.badge,.quota-title,.kv span,.noteBlock div,.cardActions button,.empty');for(const node of nodes){if(node.children.length>0)continue;if(!node.dataset.rawText)node.dataset.rawText=node.textContent;node.textContent=translateInlineText(node.dataset.rawText)}formatLocalTimes()}
function applyLocale(){document.documentElement.lang=currentLocale;document.title=t('app.title');localeSelect.value=currentLocale;for(const node of document.querySelectorAll('[data-i18n]')){node.textContent=t(node.dataset.i18n)}applyInlineTranslations();renderLogs(STATUS.logs||[])}
function changeLocale(locale){currentLocale=normalizeLocale(locale);try{window.localStorage.setItem(LOCALE_STORAGE_KEY,currentLocale)}catch(error){}applyLocale()}
function showNotice(text,isError){notice.hidden=false;notice.textContent=text;notice.className='notice'+(isError?' error':'')}
function schedulePageRefresh(delay){window.setTimeout(()=>window.location.reload(),delay||900)}
async function readJSON(resp){const text=await resp.text();if(!text)return{};try{return JSON.parse(text)}catch{return{error:text}}}
function authHeaders(){const input=document.getElementById('managementKey');const key=(input&&input.value||'').trim();if(!key)throw new Error(t('error.managementKeyRequired'));const name='Author'+'ization';const scheme='Bea'+'rer ';const headers={};headers[name]=key.toLowerCase().startsWith(scheme.toLowerCase())?key:scheme+key;return headers}
async function requestManagement(path,options){const opts=options||{};const headers=authHeaders();let url=MANAGEMENT_BASE+path;if(opts.query){const params=new URLSearchParams(opts.query);url+='?'+params.toString()}const init={method:opts.method||'GET',headers};if(Object.prototype.hasOwnProperty.call(opts,'body')){headers['Content-Type']=opts.contentType||'application/json';init.body=typeof opts.body==='string'?opts.body:JSON.stringify(opts.body)}const resp=await fetch(url,init);const data=await readJSON(resp);if(!resp.ok)throw new Error(data.error||data.message||t('error.requestFailed',{status:resp.status}));return data}
function fillSettings(){const s=STATUS.settings||{};document.getElementById('handleEnabled').checked=s.handle_enabled!==false;document.getElementById('usageFeedback').checked=s.enable_usage_feedback!==false;document.getElementById('monthlyMode').value=s.monthly_mode||'expiry_order';document.getElementById('refreshInterval').value=s.quota_refresh_interval||'30m0s';document.getElementById('staleAfter').value=s.stale_after||'5h0m0s';document.getElementById('maxConcurrency').value=s.max_refresh_concurrency||1;document.getElementById('circuitFailureThreshold').value=s.circuit_failure_threshold||3;document.getElementById('circuitOpenDuration').value=s.circuit_open_duration||'10m0s';document.getElementById('circuitHalfOpenSuccessThreshold').value=s.circuit_half_open_success_threshold||1;document.getElementById('maxLogEntries').value=s.max_log_entries||2000;document.getElementById('logRetention').value=s.log_retention||'24h0m0s'}
async function saveSettings(){try{const payload={handle_enabled:document.getElementById('handleEnabled').checked,enable_usage_feedback:document.getElementById('usageFeedback').checked,monthly_mode:document.getElementById('monthlyMode').value,quota_refresh_interval:document.getElementById('refreshInterval').value.trim(),stale_after:document.getElementById('staleAfter').value.trim(),max_refresh_concurrency:Number.parseInt(document.getElementById('maxConcurrency').value,10)||1,circuit_failure_threshold:Number.parseInt(document.getElementById('circuitFailureThreshold').value,10)||3,circuit_open_duration:document.getElementById('circuitOpenDuration').value.trim(),circuit_half_open_success_threshold:Number.parseInt(document.getElementById('circuitHalfOpenSuccessThreshold').value,10)||1,max_log_entries:Number.parseInt(document.getElementById('maxLogEntries').value,10)||2000,log_retention:document.getElementById('logRetention').value.trim()};await requestManagement('/settings',{method:'PUT',body:payload});showNotice(t('notice.settingsSaved'),false);await refreshLogs();schedulePageRefresh(700)}catch(error){showNotice(error.message||String(error),true)}}
async function refreshQuota(){try{await requestManagement('/refresh',{method:'POST'});showNotice(t('notice.refreshRequested'),false);await refreshLogs();schedulePageRefresh(1800)}catch(error){showNotice(error.message||String(error),true)}}
function splitTags(text){return text.split(',').map((item)=>item.trim()).filter(Boolean)}
function openEdit(authID){const account=accountsByID.get(authID)||{};editingAuthID=authID;document.getElementById('editAuthID').textContent=authID;document.getElementById('editAlias').value=account.alias||'';document.getElementById('editNotes').value=account.notes||'';document.getElementById('editGroupID').value=account.group_id||'';document.getElementById('editGroupName').value=account.group||'';document.getElementById('editGroupNotes').value=account.group_notes||'';document.getElementById('editTags').value=(account.tags||[]).join(', ');editDialog.showModal()}
function fillGroupFromID(){const groupID=document.getElementById('editGroupID').value.trim();const group=groupsByID.get(groupID);if(!group)return;if(!document.getElementById('editGroupName').value.trim())document.getElementById('editGroupName').value=group.name||'';if(!document.getElementById('editGroupNotes').value.trim())document.getElementById('editGroupNotes').value=group.notes||''}
async function saveAccountModal(){if(!editingAuthID)return;const groupID=document.getElementById('editGroupID').value.trim();const groupName=document.getElementById('editGroupName').value.trim();const groupNotes=document.getElementById('editGroupNotes').value.trim();try{await requestManagement('/annotations/account',{method:'PATCH',body:{auth_id:editingAuthID,alias:document.getElementById('editAlias').value,notes:document.getElementById('editNotes').value,tags:splitTags(document.getElementById('editTags').value),group_id:groupID}});const existingGroup=groupsByID.get(groupID)||{name:'',notes:''};if(groupID&&(groupName!==existingGroup.name||groupNotes!==existingGroup.notes)){await requestManagement('/annotations/group',{method:'PATCH',body:{id:groupID,name:groupName,notes:groupNotes}});groupsByID.set(groupID,{name:groupName,notes:groupNotes})}showNotice(t('notice.accountSaved'),false);editDialog.close();await refreshLogs();schedulePageRefresh(700)}catch(error){showNotice(error.message||String(error),true)}}
async function refreshOneQuota(authID){if(!authID)return;try{await requestManagement('/refresh/account',{method:'POST',body:{auth_id:authID}});showNotice(t('notice.refreshOneRequested'),false);await refreshLogs();schedulePageRefresh(1800)}catch(error){showNotice(error.message||String(error),true)}}
async function exportConfig(){try{const data=await requestManagement('/export');const blob=new Blob([JSON.stringify(data,null,2)+'\n'],{type:'application/json'});const link=document.createElement('a');link.href=URL.createObjectURL(blob);link.download='codex-quota-scheduler-config.json';link.click();URL.revokeObjectURL(link.href);showNotice(t('notice.configExported'),false);await refreshLogs()}catch(error){showNotice(error.message||String(error),true)}}
async function exportLogs(){try{const data=await requestManagement('/logs');const payload={plugin_id:STATUS.plugin_id||'codex-quota-scheduler',exported_at:new Date().toISOString(),logs:data.logs||[]};const blob=new Blob([JSON.stringify(payload,null,2)+'\n'],{type:'application/json'});const link=document.createElement('a');link.href=URL.createObjectURL(blob);link.download='codex-quota-scheduler-logs.json';link.click();URL.revokeObjectURL(link.href);showNotice(t('notice.logsExported'),false)}catch(error){showNotice(error.message||String(error),true)}}
async function importConfigFile(file){if(!file)return;try{const text=await file.text();await requestManagement('/import',{method:'POST',body:text});showNotice(t('notice.configImported'),false);await refreshLogs();schedulePageRefresh(700)}catch(error){showNotice(error.message||String(error),true)}}
function formatLogTime(value){if(!value)return'';const date=new Date(value);if(Number.isNaN(date.getTime()))return value;return date.toLocaleString('zh-CN',{hour12:false})}
function formatLocalTimes(){for(const node of document.querySelectorAll('.localTime[data-time]')){const raw=node.dataset.time||'';const date=new Date(raw);if(!Number.isNaN(date.getTime()))node.textContent=date.toLocaleString('zh-CN',{hour12:false})}}
function localizedLogMessage(log){if(currentLocale!=='en')return log.message||'';return t('log.'+(log.event||''))||translateInlineText(log.message||'')}
function renderLogs(logs){const list=document.getElementById('logList');list.replaceChildren();const items=(logs||[]).slice().reverse().slice(0,80);if(items.length===0){const empty=document.createElement('div');empty.className='empty';empty.textContent=t('logs.empty');list.appendChild(empty);return}for(const log of items){const row=document.createElement('div');row.className='logItem '+(log.level||'info');const meta=document.createElement('div');meta.className='logMeta';meta.textContent=[formatLogTime(log.time),log.event].filter(Boolean).join(' · ');const msg=document.createElement('div');msg.className='logMsg';const fields=log.fields?Object.entries(log.fields).map(([key,value])=>key+'='+value).join(currentLocale==='en'?', ':'，'):'';msg.textContent=fields?(localizedLogMessage(log)+'（'+fields+'）'):localizedLogMessage(log);row.append(meta,msg);list.appendChild(row)}}
async function refreshLogs(){try{const data=await requestManagement('/logs');renderLogs(data.logs||[])}catch(error){showNotice(error.message||String(error),true)}}
localeSelect.addEventListener('change',()=>changeLocale(localeSelect.value));
document.getElementById('saveSettings').addEventListener('click',saveSettings);
document.getElementById('refreshQuota').addEventListener('click',refreshQuota);
document.getElementById('refreshLogs').addEventListener('click',refreshLogs);
document.getElementById('exportLogs').addEventListener('click',exportLogs);
document.getElementById('exportConfig').addEventListener('click',exportConfig);
document.getElementById('importConfig').addEventListener('click',()=>document.getElementById('importFile').click());
document.getElementById('importFile').addEventListener('change',(event)=>importConfigFile(event.target.files&&event.target.files[0]));
document.getElementById('saveAccount').addEventListener('click',saveAccountModal);
document.getElementById('editGroupID').addEventListener('input',fillGroupFromID);
document.getElementById('editGroupID').addEventListener('blur',fillGroupFromID);
document.getElementById('closeDialog').addEventListener('click',()=>editDialog.close());
document.getElementById('cancelEdit').addEventListener('click',()=>editDialog.close());
for(const button of document.querySelectorAll('.openEdit')){button.addEventListener('click',()=>openEdit(button.dataset.authId||''))}
for(const button of document.querySelectorAll('.refreshOne')){button.addEventListener('click',()=>refreshOneQuota(button.dataset.authId||''))}
fillSettings();
formatLocalTimes();
applyLocale();
</script>
</body>
</html>`))

func jsonForTemplate(v any) template.JS {
	raw, err := json.Marshal(v)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(raw)
}
