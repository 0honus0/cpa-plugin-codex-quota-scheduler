package main

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http"
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
	Available         bool          `json:"available"`
	UnavailableReason string        `json:"unavailable_reason,omitempty"`
	ResetExpiry       time.Time     `json:"reset_expiry,omitempty"`
	ResetExpiryText   string        `json:"reset_expiry_text,omitempty"`
	CacheAge          string        `json:"cache_age,omitempty"`
	CacheAgeSeconds   int64         `json:"cache_age_seconds,omitempty"`
}

func RegisterManagement() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        "/status",
				Menu:        "Status",
				Description: "Scheduler quota status.",
			},
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
		return jsonManagementResponse(http.StatusOK, SettingsFromConfig(store.Config()))
	case method == http.MethodPut && path == "/settings":
		return handlePutSettings(store, req)
	case method == http.MethodPost && path == "/refresh":
		triggerRefreshSoon()
		return jsonManagementResponse(http.StatusAccepted, map[string]bool{"ok": true})
	case method == http.MethodGet && path == "/annotations":
		return jsonManagementResponse(http.StatusOK, store.Annotations())
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
			Available:         scheduled.Available,
			UnavailableReason: scheduled.UnavailableReason,
			ResetExpiry:       scheduled.SortTime,
			ResetExpiryText:   formatTime(scheduled.SortTime),
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

func RenderStatusHTML(payload StatusPayload) []byte {
	var buf bytes.Buffer
	if err := statusTemplate.Execute(&buf, payload); err != nil {
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

func handlePatchGroupAnnotation(store *PluginState, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var patch annotationPatch
	if err := json.Unmarshal(req.Body, &patch); err != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
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

func normalizeManagementPath(path string) string {
	for _, prefix := range []string{
		"/v0/management" + managementBasePath,
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

func jsonForTemplate(v any) template.JS {
	raw, err := json.Marshal(v)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(raw)
}
