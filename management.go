package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const managementBasePath = "/plugins/" + PluginID

var managementRefreshSoon = func() {}

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
	Logs          []LogEntry      `json:"logs"`
}

type SettingsPayload struct {
	HandleEnabled         bool        `json:"handle_enabled"`
	MonthlyMode           MonthlyMode `json:"monthly_mode"`
	QuotaRefreshInterval  string      `json:"quota_refresh_interval"`
	StaleAfter            string      `json:"stale_after"`
	EnableUsageFeedback   bool        `json:"enable_usage_feedback"`
	MaxRefreshConcurrency int         `json:"max_refresh_concurrency"`
	QuotaEndpoint         string      `json:"quota_endpoint"`
}

type StatusAccount struct {
	Rank              int           `json:"rank"`
	AuthID            string        `json:"auth_id"`
	Alias             string        `json:"alias,omitempty"`
	Notes             string        `json:"notes,omitempty"`
	GroupID           string        `json:"group_id,omitempty"`
	Group             string        `json:"group,omitempty"`
	GroupNotes        string        `json:"group_notes,omitempty"`
	Tags              []string      `json:"tags,omitempty"`
	CPAPriority       int           `json:"cpa_priority"`
	Family            AccountFamily `json:"family"`
	QueueStatus       QueueStatus   `json:"queue_status"`
	Available         bool          `json:"available"`
	UnavailableReason string        `json:"unavailable_reason,omitempty"`
	ResetExpiry       time.Time     `json:"reset_expiry,omitempty"`
	ResetExpiryText   string        `json:"reset_expiry_text,omitempty"`
	CacheAge          string        `json:"cache_age,omitempty"`
	CacheAgeSeconds   int64         `json:"cache_age_seconds,omitempty"`
	FiveHour          StatusWindow  `json:"five_hour"`
	LongWindow        StatusWindow  `json:"long_window"`
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
				Description: "Scheduler quota status.",
			},
			{Path: "/settings", Description: "Resource settings endpoint."},
			{Path: "/refresh", Description: "Resource refresh endpoint."},
			{Path: "/annotations/account", Description: "Resource account annotation endpoint."},
			{Path: "/annotations/group", Description: "Resource group annotation endpoint."},
			{Path: "/logs", Description: "Resource scheduler logs endpoint."},
		},
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: managementBasePath + "/status", Description: "Scheduler quota status."},
			{Method: http.MethodGet, Path: managementBasePath + "/settings", Description: "Read scheduler settings."},
			{Method: http.MethodPut, Path: managementBasePath + "/settings", Description: "Update scheduler settings."},
			{Method: http.MethodPost, Path: managementBasePath + "/refresh", Description: "Refresh quota status soon."},
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
	switch {
	case method == http.MethodGet && path == "/status":
		return handleStatusRequest(store, req, now)
	case method == http.MethodGet && path == "/settings":
		if req.Query.Get("save") == "1" {
			return handlePutSettingsFromQuery(store, req.Query, now)
		}
		return jsonManagementResponse(http.StatusOK, SettingsFromConfig(store.Config()))
	case method == http.MethodPut && path == "/settings":
		return handlePutSettings(store, req)
	case method == http.MethodPost && path == "/refresh":
		triggerRefreshSoon()
		return jsonManagementResponse(http.StatusAccepted, map[string]bool{"ok": true})
	case method == http.MethodGet && path == "/refresh":
		triggerRefreshSoon()
		store.RecordLog("info", "ui.refresh_requested", "页面请求刷新额度", nil, now)
		return jsonManagementResponse(http.StatusAccepted, map[string]bool{"ok": true})
	case method == http.MethodGet && path == "/logs":
		return jsonManagementResponse(http.StatusOK, map[string]any{"logs": store.Snapshot(now).Logs})
	case method == http.MethodGet && path == "/annotations":
		return jsonManagementResponse(http.StatusOK, store.Annotations())
	case method == http.MethodGet && path == "/annotations/account":
		return handlePatchAccountAnnotationFromQuery(store, req.Query, now)
	case method == http.MethodGet && path == "/annotations/group":
		return handlePatchGroupAnnotationFromQuery(store, req.Query, now)
	case method == http.MethodPut && path == "/annotations":
		return handlePutAnnotations(store, req)
	case method == http.MethodPatch && path == "/annotations/account":
		return handlePatchAccountAnnotation(store, req)
	case method == http.MethodPatch && path == "/annotations/group":
		return handlePatchGroupAnnotation(store, req)
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func SettingsFromConfig(cfg Config) SettingsPayload {
	return SettingsPayload{
		HandleEnabled:         cfg.HandleEnabled,
		MonthlyMode:           cfg.MonthlyMode,
		QuotaRefreshInterval:  cfg.QuotaRefreshInterval.String(),
		StaleAfter:            cfg.StaleAfter.String(),
		EnableUsageFeedback:   cfg.EnableUsageFeedback,
		MaxRefreshConcurrency: cfg.MaxRefreshConcurrency,
		QuotaEndpoint:         cfg.QuotaEndpoint,
	}
}

func ConfigFromSettings(base Config, payload SettingsPayload) (Config, error) {
	cfg := base
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
		cfg.QuotaEndpoint = strings.TrimSpace(payload.QuotaEndpoint)
	}
	return cfg, nil
}

type jsonError string

func (e jsonError) Error() string { return string(e) }

func handlePutSettings(store *PluginState, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var payload SettingsPayload
	if err := json.Unmarshal(req.Body, &payload); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
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

func handlePutSettingsFromQuery(store *PluginState, query url.Values, now time.Time) pluginapi.ManagementResponse {
	payload := SettingsPayload{
		HandleEnabled:         queryBool(query, "handle_enabled", true),
		MonthlyMode:           MonthlyMode(query.Get("monthly_mode")),
		QuotaRefreshInterval:  query.Get("quota_refresh_interval"),
		StaleAfter:            query.Get("stale_after"),
		EnableUsageFeedback:   queryBool(query, "enable_usage_feedback", true),
		MaxRefreshConcurrency: queryInt(query, "max_refresh_concurrency", 4),
		QuotaEndpoint:         query.Get("quota_endpoint"),
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

func queryBool(query url.Values, key string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(query.Get(key)))
	if raw == "" {
		return fallback
	}
	return raw == "1" || raw == "true" || raw == "yes" || raw == "on"
}

func queryInt(query url.Values, key string, fallback int) int {
	raw := strings.TrimSpace(query.Get(key))
	if raw == "" {
		return fallback
	}
	var out int
	if _, err := fmt.Sscanf(raw, "%d", &out); err != nil || out == 0 {
		return fallback
	}
	return out
}

func queryStringPtr(query url.Values, key string) *string {
	if _, ok := query[key]; !ok {
		return nil
	}
	value := query.Get(key)
	return &value
}

func splitQueryTags(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	tags := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			tags = append(tags, part)
		}
	}
	return tags
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
		Logs:          cloneLogs(snapshot.Logs),
	}
	for i, scheduled := range ordered {
		if payload.NextAuthID == "" && scheduled.Available {
			payload.NextAuthID = scheduled.AuthID
		}
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
			FiveHour:          statusWindow(account.Quota.FiveHour, "5 小时额度"),
			LongWindow:        statusWindow(account.Quota.LongWindow, longWindowLabelCN(account.Family)),
		}
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

func statusWindow(window *QuotaWindow, label string) StatusWindow {
	if window == nil {
		return StatusWindow{Label: label, DisplayText: "暂无数据", Missing: true}
	}
	used := 0.0
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
	if window.Exhausted {
		remaining = 0
	}
	displayText := fmt.Sprintf("剩余 %.0f%%", remaining)
	if window.Exhausted {
		displayText = "已用完"
	}
	return StatusWindow{
		Kind:             window.Kind,
		Label:            label,
		UsedPercent:      used,
		RemainingPercent: remaining,
		DisplayText:      displayText,
		Exhausted:        window.Exhausted,
		ResetAt:          window.ResetAt,
		ResetText:        formatTime(window.ResetAt),
	}
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
	if req.Query.Get("format") == "json" {
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

func handlePatchAccountAnnotation(store *PluginState, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var patch annotationPatch
	if err := json.Unmarshal(req.Body, &patch); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	return applyAccountAnnotationPatch(store, patch)
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

func handlePatchAccountAnnotationFromQuery(store *PluginState, query url.Values, now time.Time) pluginapi.ManagementResponse {
	patch := annotationPatch{
		Key:     query.Get("key"),
		ID:      query.Get("id"),
		AuthID:  query.Get("auth_id"),
		Alias:   queryStringPtr(query, "alias"),
		Notes:   queryStringPtr(query, "notes"),
		Tags:    splitQueryTags(query.Get("tags")),
		GroupID: queryStringPtr(query, "group_id"),
	}
	resp := applyAccountAnnotationPatch(store, patch)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		store.RecordLog("info", "ui.account_saved", "页面保存账号卡片", map[string]any{"auth_id": patch.AuthID}, now)
	}
	return resp
}

func handlePatchGroupAnnotation(store *PluginState, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var patch annotationPatch
	if err := json.Unmarshal(req.Body, &patch); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	return applyGroupAnnotationPatch(store, patch)
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

func handlePatchGroupAnnotationFromQuery(store *PluginState, query url.Values, now time.Time) pluginapi.ManagementResponse {
	patch := annotationPatch{
		Key:   query.Get("key"),
		ID:    query.Get("id"),
		Name:  queryStringPtr(query, "name"),
		Notes: queryStringPtr(query, "notes"),
		Tags:  splitQueryTags(query.Get("tags")),
		Color: queryStringPtr(query, "color"),
	}
	resp := applyGroupAnnotationPatch(store, patch)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		store.RecordLog("info", "ui.group_saved", "页面保存账号分组", map[string]any{"group_id": patch.ID}, now)
	}
	return resp
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

var statusTemplate = template.Must(template.New("status").Funcs(template.FuncMap{
	"join": strings.Join,
	"json": jsonForTemplate,
}).Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Codex 额度调度器</title>
<style>
*{box-sizing:border-box}body{font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;color:#1f2937;background:#f6f7f9}button,input,select,textarea{font:inherit}button{border:0;border-radius:7px;padding:9px 12px;background:#2563eb;color:white;cursor:pointer;font-weight:650}button.secondary{background:#eef2ff;color:#1e40af}button:disabled{opacity:.55;cursor:not-allowed}code{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:12px}.shell{display:grid;grid-template-columns:300px minmax(0,1fr);min-height:100vh}.sidebar{background:#fff;border-right:1px solid #e5e7eb;padding:18px;position:sticky;top:0;height:100vh;overflow:auto}.main{padding:20px 22px 32px;overflow:auto}.brand{display:grid;gap:4px;margin-bottom:18px}.brand h1{font-size:20px;line-height:1.2;margin:0;color:#111827}.brand p{font-size:12px;line-height:1.45;color:#6b7280;margin:0}.section{border-top:1px solid #eef0f3;padding-top:16px;margin-top:16px}.section h2{font-size:14px;margin:0 0 12px;color:#111827}.field{display:grid;gap:6px;margin-bottom:12px}.field span{font-size:12px;color:#4b5563;font-weight:650}.field input,.field select,.field textarea{width:100%;border:1px solid #d1d5db;border-radius:7px;background:white;color:#111827;padding:8px 10px}.field textarea{min-height:74px;resize:vertical}.toggle{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:12px}.toggle span{font-size:13px;font-weight:650}.actions{display:flex;gap:8px;flex-wrap:wrap}.notice{margin-top:12px;border-radius:7px;padding:10px 11px;background:#ecfdf5;color:#065f46;font-size:12px;line-height:1.45}.notice.error{background:#fef2f2;color:#991b1b}.toolbar{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:18px}.toolbar h2{font-size:22px;margin:0;color:#111827}.toolbar p{font-size:13px;color:#6b7280;margin:5px 0 0}.metrics{display:flex;gap:8px;flex-wrap:wrap;justify-content:flex-end}.metric{border:1px solid #e5e7eb;background:#fff;border-radius:7px;padding:8px 10px;font-size:12px;color:#374151}.queue{display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:12px}.card{background:#fff;border:1px solid #e5e7eb;border-radius:8px;padding:14px;display:grid;gap:12px;box-shadow:0 1px 2px rgba(15,23,42,.04)}.card.next{border-color:#2563eb;box-shadow:0 0 0 1px rgba(37,99,235,.18),0 8px 24px rgba(37,99,235,.08)}.cardTop{display:flex;align-items:flex-start;justify-content:space-between;gap:10px}.rank{display:inline-flex;align-items:center;justify-content:center;min-width:30px;height:30px;border-radius:7px;background:#111827;color:#fff;font-weight:750;font-size:13px}.identity{min-width:0;display:grid;gap:4px}.title{font-weight:750;color:#111827;overflow-wrap:anywhere}.sub{font-size:12px;color:#6b7280;overflow-wrap:anywhere}.badges{display:flex;gap:6px;flex-wrap:wrap}.badge{border-radius:999px;background:#f3f4f6;color:#374151;padding:4px 8px;font-size:12px}.badge.ok{background:#dcfce7;color:#166534}.badge.no{background:#fee2e2;color:#991b1b}.badge.next{background:#dbeafe;color:#1d4ed8}.kv{display:grid;grid-template-columns:88px minmax(0,1fr);gap:6px 10px;font-size:12px}.kv span:nth-child(odd){color:#6b7280}.kv span:nth-child(even){color:#111827;overflow-wrap:anywhere}.chips{display:flex;gap:6px;flex-wrap:wrap;min-height:24px}.chip{border-radius:999px;background:#eef2ff;color:#3730a3;padding:4px 8px;font-size:12px}details.editor{border-top:1px solid #eef0f3;padding-top:10px}details.editor summary{cursor:pointer;color:#2563eb;font-size:13px;font-weight:700}.editorGrid{display:grid;gap:9px;margin-top:10px}.empty{background:#fff;border:1px dashed #d1d5db;border-radius:8px;padding:28px;text-align:center;color:#6b7280}@media(max-width:860px){.shell{grid-template-columns:1fr}.sidebar{height:auto;position:relative;border-right:0;border-bottom:1px solid #e5e7eb}.toolbar{display:grid}.metrics{justify-content:flex-start}}
</style>
</head>
<body>
<div class="shell"><aside class="sidebar"><div class="brand"><h1>Codex 额度调度器</h1><p>优化版 Fill First。配置和账号备注由插件内置状态文件保存，不在 CPA 配置字段中展示。</p></div><div class="section"><h2>调度设置</h2><label class="field"><span>CPA 管理密钥（保存时需要）</span><input id="managementKey" type="password" autocomplete="off" placeholder="粘贴管理密钥"></label><label class="toggle"><span>启用调度接管</span><input id="handleEnabled" type="checkbox"></label><label class="toggle"><span>失败反馈标记额度耗尽</span><input id="usageFeedback" type="checkbox"></label><label class="field"><span>Monthly 模式</span><select id="monthlyMode"><option value="expiry_order">按到期时间排序</option><option value="priority">优先使用 Monthly</option></select></label><label class="field"><span>额度刷新间隔</span><input id="refreshInterval" spellcheck="false"></label><label class="field"><span>缓存过期判定</span><input id="staleAfter" spellcheck="false"></label><label class="field"><span>最大并发刷新</span><input id="maxConcurrency" type="number" min="1" step="1"></label><div class="actions"><button id="saveSettings" type="button">保存设置</button><button id="refreshQuota" type="button" class="secondary">刷新额度</button></div><div id="notice" class="notice" hidden></div></div></aside><main class="main"><div class="toolbar"><div><h2>账号队列</h2><p>账号卡片按当前内部调度优先级排序。第一个可用账号就是下一次 Codex 请求会优先选择的账号。</p></div><div class="metrics"><span class="metric">下一账号：<code>{{if .NextAuthID}}{{.NextAuthID}}{{else}}暂无{{end}}</code></span><span class="metric">Monthly：{{if eq .MonthlyMode "priority"}}优先使用{{else}}按到期时间{{end}}</span><span class="metric">调度：{{if .HandleEnabled}}已启用{{else}}已关闭{{end}}</span><span class="metric">最近选择：<code>{{if .LastSelected}}{{.LastSelected}}{{else}}暂无{{end}}</code></span></div></div><section class="queue" aria-label="账号卡片">{{range .Accounts}}<article class="card {{if and $.NextAuthID (eq $.NextAuthID .AuthID)}}next{{end}}" data-auth-id="{{.AuthID}}" data-group-id="{{.GroupID}}"><div class="cardTop"><div class="identity"><div class="title">{{if .Alias}}{{.Alias}}{{else}}{{.AuthID}}{{end}}</div><div class="sub"><code>{{.AuthID}}</code></div></div><span class="rank">#{{.Rank}}</span></div><div class="badges">{{if and $.NextAuthID (eq $.NextAuthID .AuthID)}}<span class="badge next">下一优先</span>{{end}}{{if .Available}}<span class="badge ok">可用</span>{{else}}<span class="badge no">{{.UnavailableReason}}</span>{{end}}<span class="badge">{{if eq .Family "weekly"}}Weekly{{else if eq .Family "monthly"}}Monthly{{else}}未知类型{{end}}</span><span class="badge">CPA 优先级 {{.CPAPriority}}</span></div><div class="kv"><span>重置时间</span><span>{{if .ResetExpiryText}}{{.ResetExpiryText}}{{else}}未知{{end}}</span><span>缓存时间</span><span>{{if .CacheAge}}{{.CacheAge}}{{else}}暂无{{end}}</span><span>分组</span><span>{{if .Group}}{{.Group}}{{else}}未分组{{end}}</span><span>标签</span><span>{{if .Tags}}{{join .Tags ", "}}{{else}}无{{end}}</span></div><div class="chips">{{range .Tags}}<span class="chip">{{.}}</span>{{end}}</div><details class="editor"><summary>编辑账号卡片</summary><div class="editorGrid"><label class="field"><span>别名</span><input class="aliasInput" value="{{.Alias}}"></label><label class="field"><span>账号备注</span><textarea class="notesInput">{{.Notes}}</textarea></label><label class="field"><span>分组 ID</span><input class="groupInput" value="{{.GroupID}}" placeholder="team-a"></label><label class="field"><span>分组名称</span><input class="groupNameInput" value="{{.Group}}"></label><label class="field"><span>标签（逗号分隔）</span><input class="tagsInput" value="{{join .Tags ", "}}"></label><button type="button" class="saveAccount">保存账号</button></div></details></article>{{else}}<div class="empty">暂无账号数据。等待额度刷新后，这里会显示账号卡片。</div>{{end}}</section></main></div><script>const STATUS={{json .}};const notice=document.getElementById('notice');const keyInput=document.getElementById('managementKey');function showNotice(text,isError){notice.hidden=false;notice.textContent=text;notice.className='notice'+(isError?' error':'')}function headers(jsonBody){const h={};const key=keyInput.value.trim();const marker='be'+'arer ';const headerName='Author'+'ization';if(key)h[headerName]=key.toLowerCase().startsWith(marker)?key:('Be'+'arer '+key);if(jsonBody)h['Content-Type']='application/json';return h}async function readJSON(resp){const text=await resp.text();if(!text)return{};try{return JSON.parse(text)}catch{return{error:text}}}async function request(path,options){const resp=await fetch(path,options||{});const data=await readJSON(resp);if(!resp.ok)throw new Error(data.error||data.message||('请求失败：'+resp.status));return data}function fillSettings(){const s=STATUS.settings||{};document.getElementById('handleEnabled').checked=s.handle_enabled!==false;document.getElementById('usageFeedback').checked=s.enable_usage_feedback!==false;document.getElementById('monthlyMode').value=s.monthly_mode||'expiry_order';document.getElementById('refreshInterval').value=s.quota_refresh_interval||'1m0s';document.getElementById('staleAfter').value=s.stale_after||'10m0s';document.getElementById('maxConcurrency').value=s.max_refresh_concurrency||4}async function saveSettings(){try{const payload={handle_enabled:document.getElementById('handleEnabled').checked,enable_usage_feedback:document.getElementById('usageFeedback').checked,monthly_mode:document.getElementById('monthlyMode').value,quota_refresh_interval:document.getElementById('refreshInterval').value.trim(),stale_after:document.getElementById('staleAfter').value.trim(),max_refresh_concurrency:Number.parseInt(document.getElementById('maxConcurrency').value,10)||1};await request('/v0/management/plugins/codex-quota-scheduler/settings',{method:'PUT',headers:headers(true),body:JSON.stringify(payload)});showNotice('设置已保存。刷新页面后可看到最新排序。',false)}catch(error){showNotice(error.message||String(error),true)}}async function refreshQuota(){try{await request('/v0/management/plugins/codex-quota-scheduler/refresh',{method:'POST',headers:headers(false)});showNotice('已请求后台刷新额度。稍后刷新页面查看结果。',false)}catch(error){showNotice(error.message||String(error),true)}}function splitTags(text){return text.split(',').map((item)=>item.trim()).filter(Boolean)}async function saveAccount(card){const authID=card.dataset.authId||'';const groupID=card.querySelector('.groupInput').value.trim();const groupName=card.querySelector('.groupNameInput').value.trim();try{await request('/v0/management/plugins/codex-quota-scheduler/annotations/account',{method:'PATCH',headers:headers(true),body:JSON.stringify({auth_id:authID,alias:card.querySelector('.aliasInput').value,notes:card.querySelector('.notesInput').value,tags:splitTags(card.querySelector('.tagsInput').value),group_id:groupID})});if(groupID&&groupName){await request('/v0/management/plugins/codex-quota-scheduler/annotations/group',{method:'PATCH',headers:headers(true),body:JSON.stringify({id:groupID,name:groupName})})}showNotice('账号卡片已保存。刷新页面后可看到最新显示。',false)}catch(error){showNotice(error.message||String(error),true)}}document.getElementById('saveSettings').addEventListener('click',saveSettings);document.getElementById('refreshQuota').addEventListener('click',refreshQuota);for(const button of document.querySelectorAll('.saveAccount')){button.addEventListener('click',()=>saveAccount(button.closest('.card')))}fillSettings();</script>
</body>
</html>`))

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
<div class="shell" data-resource-settings="/v0/resource/plugins/codex-quota-scheduler/settings" data-resource-logs="/v0/resource/plugins/codex-quota-scheduler/logs">
<aside class="sidebar">
<div class="brand"><h1>Codex 额度调度器</h1><p>优化版 Fill First。配置、别名、分组、标签和备注由插件内部状态文件保存。</p></div>
<div class="section"><h2>调度设置</h2>
<label class="toggle"><span>启用调度接管</span><input id="handleEnabled" type="checkbox"></label>
<label class="toggle"><span>失败反馈标记额度耗尽</span><input id="usageFeedback" type="checkbox"></label>
<label class="field"><span>Monthly 模式</span><select id="monthlyMode"><option value="expiry_order">按到期时间排序</option><option value="priority">优先使用 Monthly</option></select></label>
<label class="field"><span>额度刷新间隔</span><input id="refreshInterval" spellcheck="false"></label>
<label class="field"><span>缓存过期判定</span><input id="staleAfter" spellcheck="false"></label>
<label class="field"><span>最大并发刷新</span><input id="maxConcurrency" type="number" min="1" step="1"></label>
<div class="actions"><button id="saveSettings" type="button">保存设置</button><button id="refreshQuota" type="button" class="secondary">刷新额度</button></div>
<div id="notice" class="notice" hidden></div>
</div>
</aside>
<main class="main">
<div class="toolbar"><div><h2>账号队列</h2><p>账号卡片按当前调度优先级排序。第一个可用账号就是下一次 Codex 请求会优先选择的账号。</p></div><div class="metrics"><span class="metric">下一账号：<code>{{if .NextAuthID}}{{.NextAuthID}}{{else}}暂无{{end}}</code></span><span class="metric">Monthly：{{if eq .MonthlyMode "priority"}}优先使用{{else}}按到期时间{{end}}</span><span class="metric">调度：{{if .HandleEnabled}}已启用{{else}}已关闭{{end}}</span><span class="metric">最近选择：<code>{{if .LastSelected}}{{.LastSelected}}{{else}}暂无{{end}}</code></span></div></div>
<section class="queue" aria-label="账号卡片">{{range .Accounts}}<article class="card {{if and $.NextAuthID (eq $.NextAuthID .AuthID)}}next{{end}}" data-auth-id="{{.AuthID}}">
<div class="cardTop"><div class="identity"><div class="titleLine"><span class="title">{{if .Alias}}{{.Alias}}{{else}}{{.AuthID}}{{end}}</span>{{if .Group}}<span class="groupPill">{{.Group}}</span>{{end}}</div><div class="sub"><code>{{.AuthID}}</code></div>{{if .Tags}}<div class="metaLine">{{range .Tags}}<span class="chip">{{.}}</span>{{end}}</div>{{end}}</div><span class="rank">#{{.Rank}}</span></div>
<div class="badges">{{if and $.NextAuthID (eq $.NextAuthID .AuthID)}}<span class="badge next">下一优先</span>{{end}}{{if .Available}}<span class="badge ok">可用</span>{{else}}<span class="badge no">{{.UnavailableReason}}</span>{{end}}<span class="badge">{{if eq .Family "weekly"}}Weekly{{else if eq .Family "monthly"}}Monthly{{else}}未知类型{{end}}</span><span class="badge">CPA 优先级 {{.CPAPriority}}</span></div>
<div class="quotaList">{{if not .FiveHour.Missing}}<div class="quota-row"><div class="quota-head"><span class="quota-title">{{.FiveHour.Label}}</span><span>{{.FiveHour.DisplayText}}</span><span class="quota-reset">5 小时重置：<span class="localTime" data-time="{{.FiveHour.ResetText}}">{{.FiveHour.ResetText}}</span></span></div><div class="quota-bar"><div class="quota-fill {{if .FiveHour.Exhausted}}danger{{else if le .FiveHour.RemainingPercent 20.0}}warn{{end}}" style="width:{{printf "%.0f" .FiveHour.RemainingPercent}}%"></div></div></div>{{end}}<div class="quota-row"><div class="quota-head"><span class="quota-title">{{.LongWindow.Label}}</span><span>{{.LongWindow.DisplayText}}</span>{{if not .LongWindow.Missing}}<span class="quota-reset">长额度重置：<span class="localTime" data-time="{{.LongWindow.ResetText}}">{{.LongWindow.ResetText}}</span></span>{{end}}</div><div class="quota-bar"><div class="quota-fill {{if .LongWindow.Exhausted}}danger{{else if le .LongWindow.RemainingPercent 20.0}}warn{{end}}" style="width:{{printf "%.0f" .LongWindow.RemainingPercent}}%"></div></div></div></div>
<div class="kv"><span>缓存时间</span><span>{{if .CacheAge}}{{.CacheAge}}{{else}}暂无{{end}}</span></div>
{{if or .Notes .GroupNotes}}<div class="noteBlock">{{if .Notes}}<div>账号备注：{{.Notes}}</div>{{end}}{{if .GroupNotes}}<div>分组备注：{{.GroupNotes}}</div>{{end}}</div>{{end}}
<div class="cardActions"><button type="button" class="secondary openEdit" data-auth-id="{{.AuthID}}">编辑</button></div>
</article>{{else}}<div class="empty">暂无账号数据。等待额度刷新后，这里会显示账号卡片。</div>{{end}}</section>
<section class="logs"><div class="logsHeader"><h2>调度日志</h2><button id="refreshLogs" type="button" class="ghost">刷新日志</button></div><div id="logList" class="logList"></div></section>
</main>
</div>
<dialog id="editDialog"><form method="dialog" class="dialogBody"><div class="dialogHead"><div><h2>编辑账号</h2><p id="editAuthID"></p></div><button type="button" id="closeDialog" class="ghost">关闭</button></div><div class="dialogGrid"><label class="field"><span>别名</span><input id="editAlias"></label><label class="field"><span>分组 ID</span><input id="editGroupID" placeholder="team-a"></label><label class="field"><span>分组名称</span><input id="editGroupName"></label><label class="field"><span>标签</span><input id="editTags" placeholder="team, paid"></label><label class="field wide"><span>账号备注</span><textarea id="editNotes"></textarea></label><label class="field wide"><span>分组备注</span><textarea id="editGroupNotes"></textarea></label></div><div class="dialogActions"><button type="button" id="saveAccount" class="secondary">保存账号</button><button type="button" id="cancelEdit" class="ghost">取消</button></div></form></dialog>
<script>
const STATUS={{json .}};
const RESOURCE_BASE='/v0/resource/plugins/codex-quota-scheduler';
const notice=document.getElementById('notice');
const editDialog=document.getElementById('editDialog');
const accountsByID=new Map((STATUS.accounts||[]).map((account)=>[account.auth_id,account]));
let editingAuthID='';
function showNotice(text,isError){notice.hidden=false;notice.textContent=text;notice.className='notice'+(isError?' error':'')}
async function readJSON(resp){const text=await resp.text();if(!text)return{};try{return JSON.parse(text)}catch{return{error:text}}}
async function requestResource(path,params){const query=new URLSearchParams(params||{});const suffix=query.toString()?path+'?'+query.toString():path;const resp=await fetch(RESOURCE_BASE+suffix);const data=await readJSON(resp);if(!resp.ok)throw new Error(data.error||data.message||('请求失败：'+resp.status));return data}
function fillSettings(){const s=STATUS.settings||{};document.getElementById('handleEnabled').checked=s.handle_enabled!==false;document.getElementById('usageFeedback').checked=s.enable_usage_feedback!==false;document.getElementById('monthlyMode').value=s.monthly_mode||'expiry_order';document.getElementById('refreshInterval').value=s.quota_refresh_interval||'30m0s';document.getElementById('staleAfter').value=s.stale_after||'6h0m0s';document.getElementById('maxConcurrency').value=s.max_refresh_concurrency||4}
async function saveSettings(){try{await requestResource('/settings',{save:'1',handle_enabled:String(document.getElementById('handleEnabled').checked),enable_usage_feedback:String(document.getElementById('usageFeedback').checked),monthly_mode:document.getElementById('monthlyMode').value,quota_refresh_interval:document.getElementById('refreshInterval').value.trim(),stale_after:document.getElementById('staleAfter').value.trim(),max_refresh_concurrency:String(Number.parseInt(document.getElementById('maxConcurrency').value,10)||1)});showNotice('设置已保存。刷新页面后可看到最新排序。',false);await refreshLogs()}catch(error){showNotice(error.message||String(error),true)}}
async function refreshQuota(){try{await requestResource('/refresh');showNotice('已请求后台刷新额度。稍后刷新页面查看结果。',false);await refreshLogs()}catch(error){showNotice(error.message||String(error),true)}}
function splitTags(text){return text.split(',').map((item)=>item.trim()).filter(Boolean)}
function openEdit(authID){const account=accountsByID.get(authID)||{};editingAuthID=authID;document.getElementById('editAuthID').textContent=authID;document.getElementById('editAlias').value=account.alias||'';document.getElementById('editNotes').value=account.notes||'';document.getElementById('editGroupID').value=account.group_id||'';document.getElementById('editGroupName').value=account.group||'';document.getElementById('editGroupNotes').value=account.group_notes||'';document.getElementById('editTags').value=(account.tags||[]).join(', ');editDialog.showModal()}
async function saveAccountModal(){if(!editingAuthID)return;const groupID=document.getElementById('editGroupID').value.trim();try{await requestResource('/annotations/account',{auth_id:editingAuthID,alias:document.getElementById('editAlias').value,notes:document.getElementById('editNotes').value,group_id:groupID,tags:splitTags(document.getElementById('editTags').value).join(',')});if(groupID){await requestResource('/annotations/group',{id:groupID,name:document.getElementById('editGroupName').value,notes:document.getElementById('editGroupNotes').value})}showNotice('账号卡片已保存。刷新页面后可看到最新显示。',false);editDialog.close();await refreshLogs()}catch(error){showNotice(error.message||String(error),true)}}
function formatLogTime(value){if(!value)return'';const date=new Date(value);if(Number.isNaN(date.getTime()))return value;return date.toLocaleString('zh-CN',{hour12:false})}
function formatLocalTimes(){for(const node of document.querySelectorAll('.localTime[data-time]')){const raw=node.dataset.time||'';const date=new Date(raw);if(!Number.isNaN(date.getTime()))node.textContent=date.toLocaleString('zh-CN',{hour12:false})}}
function renderLogs(logs){const list=document.getElementById('logList');list.replaceChildren();const items=(logs||[]).slice().reverse().slice(0,80);if(items.length===0){const empty=document.createElement('div');empty.className='empty';empty.textContent='暂无日志。发起请求或手动刷新额度后，这里会显示记录。';list.appendChild(empty);return}for(const log of items){const row=document.createElement('div');row.className='logItem '+(log.level||'info');const meta=document.createElement('div');meta.className='logMeta';meta.textContent=[formatLogTime(log.time),log.event].filter(Boolean).join(' · ');const msg=document.createElement('div');msg.className='logMsg';const fields=log.fields?Object.entries(log.fields).map(([key,value])=>key+'='+value).join('，'):'';msg.textContent=fields?(log.message+'（'+fields+'）'):log.message;row.append(meta,msg);list.appendChild(row)}}
async function refreshLogs(){try{const data=await requestResource('/logs');renderLogs(data.logs||[])}catch(error){showNotice(error.message||String(error),true)}}
document.getElementById('saveSettings').addEventListener('click',saveSettings);
document.getElementById('refreshQuota').addEventListener('click',refreshQuota);
document.getElementById('refreshLogs').addEventListener('click',refreshLogs);
document.getElementById('saveAccount').addEventListener('click',saveAccountModal);
document.getElementById('closeDialog').addEventListener('click',()=>editDialog.close());
document.getElementById('cancelEdit').addEventListener('click',()=>editDialog.close());
for(const button of document.querySelectorAll('.openEdit')){button.addEventListener('click',()=>openEdit(button.dataset.authId||''))}
fillSettings();
formatLocalTimes();
renderLogs(STATUS.logs||[]);
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
