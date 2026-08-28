package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const managementBasePath = "/plugins/" + PluginID

func isResourcePath(path string) bool {
	return path == "/v0/resource"+managementBasePath || strings.HasPrefix(path, "/v0/resource"+managementBasePath+"/")
}

func normalizeManagementPath(path string) string {
	for _, prefix := range []string{"/v0/management" + managementBasePath, "/v0/resource" + managementBasePath, managementBasePath} {
		if path == prefix {
			return "/"
		}
		if strings.HasPrefix(path, prefix+"/") {
			return strings.TrimPrefix(path, prefix)
		}
	}
	return path
}

type CoreStatusPayload struct {
	PluginID     string              `json:"plugin_id"`
	Version      string              `json:"version"`
	GeneratedAt  time.Time           `json:"generated_at"`
	LastSelected string              `json:"last_selected,omitempty"`
	LastReason   string              `json:"last_reason,omitempty"`
	Settings     CoreSettingsPayload `json:"settings"`
	Accounts     []CoreStatusAccount `json:"accounts"`
}

type CoreStatusAccount struct {
	AuthID            string       `json:"auth_id"`
	Name              string       `json:"name,omitempty"`
	Label             string       `json:"label,omitempty"`
	Email             string       `json:"email,omitempty"`
	CPAPriority       int          `json:"cpa_priority"`
	SchedulerPriority int          `json:"scheduler_priority"`
	Alias             string       `json:"alias,omitempty"`
	RefreshEnabled    bool         `json:"refresh_enabled"`
	Disabled401       bool         `json:"disabled_401"`
	Banned            bool         `json:"banned"`
	BanUntil          time.Time    `json:"ban_until,omitempty"`
	Available         bool         `json:"available"`
	UnavailableReason string       `json:"unavailable_reason,omitempty"`
	LastRefreshAt     time.Time    `json:"last_refresh_at,omitempty"`
	LastSuccessAt     time.Time    `json:"last_success_at,omitempty"`
	LastError         string       `json:"last_error,omitempty"`
	FiveHour          *QuotaWindow `json:"five_hour,omitempty"`
	LongWindow        *QuotaWindow `json:"long_window,omitempty"`
	ProbeDueAt        time.Time    `json:"probe_due_at,omitempty"`
	LastProbeAt       time.Time    `json:"last_probe_at,omitempty"`
	ProbeStatus       string       `json:"probe_status,omitempty"`
}

func CoreRegisterManagement() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Resources: []pluginapi.ResourceRoute{{Path: "/status", Menu: "Codex 调度器", Description: "Codex quota scheduler."}},
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: managementBasePath + "/status", Description: "Scheduler status."},
			{Method: http.MethodGet, Path: managementBasePath + "/settings", Description: "Read scheduler settings."},
			{Method: http.MethodPut, Path: managementBasePath + "/settings", Description: "Update scheduler settings."},
			{Method: http.MethodPatch, Path: managementBasePath + "/account", Description: "Update one account scheduler preference."},
			{Method: http.MethodPost, Path: managementBasePath + "/refresh", Description: "Refresh all enabled scheduler accounts."},
			{Method: http.MethodPost, Path: managementBasePath + "/refresh/account", Description: "Refresh one scheduler account."},
			{Method: http.MethodGet, Path: managementBasePath + "/logs", Description: "Read scheduler logs."},
		},
	}
}

func CoreHandleManagement(engine *CoreEngine, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if engine == nil {
		return coreJSON(http.StatusServiceUnavailable, map[string]string{"error": "scheduler unavailable"})
	}
	path := normalizeManagementPath(req.Path)
	method := strings.ToUpper(req.Method)
	if isResourcePath(req.Path) {
		if method != http.MethodGet || path != "/status" {
			return coreJSON(http.StatusNotFound, map[string]string{"error": "not found"})
		}
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            []string{"text/html; charset=utf-8"},
				"Cache-Control":           []string{"no-store"},
				"X-Content-Type-Options":  []string{"nosniff"},
				"Content-Security-Policy": []string{"default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'"},
			},
			Body: []byte(coreStatusHTML),
		}
	}

	switch {
	case method == http.MethodGet && path == "/status":
		_ = engine.SyncRoster()
		return coreJSON(http.StatusOK, engine.Status())
	case method == http.MethodGet && path == "/settings":
		return coreJSON(http.StatusOK, coreSettingsFromConfig(engine.Config()))
	case method == http.MethodPut && path == "/settings":
		var payload CoreSettingsPayload
		if err := json.Unmarshal(req.Body, &payload); err != nil {
			return coreJSON(http.StatusBadRequest, map[string]string{"error": "invalid settings JSON"})
		}
		cfg, err := coreConfigFromSettings(engine.Config(), payload)
		if err != nil {
			return coreJSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		if err := engine.UpdateConfig(cfg); err != nil {
			return coreJSON(http.StatusInternalServerError, map[string]string{"error": "failed to persist settings"})
		}
		return coreJSON(http.StatusOK, coreSettingsFromConfig(engine.Config()))
	case method == http.MethodPatch && path == "/account":
		var payload struct {
			AuthID            string  `json:"auth_id"`
			RefreshEnabled    *bool   `json:"refresh_enabled"`
			SchedulerPriority *int    `json:"scheduler_priority"`
			Alias             *string `json:"alias"`
		}
		if err := json.Unmarshal(req.Body, &payload); err != nil || strings.TrimSpace(payload.AuthID) == "" {
			return coreJSON(http.StatusBadRequest, map[string]string{"error": "auth_id is required"})
		}
		if err := engine.SetAccountPreference(payload.AuthID, payload.RefreshEnabled, payload.SchedulerPriority, payload.Alias); err != nil {
			return coreJSON(http.StatusInternalServerError, map[string]string{"error": "failed to persist account preference"})
		}
		return coreJSON(http.StatusOK, map[string]bool{"ok": true})
	case method == http.MethodPost && path == "/refresh":
		if err := engine.SyncRoster(); err != nil {
			return coreJSON(http.StatusServiceUnavailable, map[string]string{"error": "failed to sync account roster"})
		}
		engine.RequestRefreshAll()
		return coreJSON(http.StatusAccepted, map[string]bool{"ok": true})
	case method == http.MethodPost && path == "/refresh/account":
		var payload struct {
			AuthID string `json:"auth_id"`
		}
		if err := json.Unmarshal(req.Body, &payload); err != nil || strings.TrimSpace(payload.AuthID) == "" {
			return coreJSON(http.StatusBadRequest, map[string]string{"error": "auth_id is required"})
		}
		if _, ok := engine.accountByID(payload.AuthID); !ok {
			return coreJSON(http.StatusNotFound, map[string]string{"error": "account not found"})
		}
		engine.RequestRefreshOne(payload.AuthID)
		return coreJSON(http.StatusAccepted, map[string]bool{"ok": true})
	case method == http.MethodGet && path == "/logs":
		return coreJSON(http.StatusOK, map[string]any{"logs": engine.Logs()})
	default:
		return coreJSON(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (e *CoreEngine) Status() CoreStatusPayload {
	now := e.now()
	cfg := e.Config()
	e.mu.RLock()
	lastSelected, lastReason := e.lastSelected, e.lastReason
	e.mu.RUnlock()
	accounts := e.Accounts()
	out := CoreStatusPayload{
		PluginID:     PluginID,
		Version:      pluginVersion,
		GeneratedAt:  now,
		LastSelected: lastSelected,
		LastReason:   lastReason,
		Settings:     coreSettingsFromConfig(cfg),
		Accounts:     make([]CoreStatusAccount, 0, len(accounts)),
	}
	for _, account := range accounts {
		out.Accounts = append(out.Accounts, CoreStatusAccount{
			AuthID:            account.ID,
			Name:              account.Name,
			Label:             account.Label,
			Email:             account.Email,
			CPAPriority:       account.Priority,
			SchedulerPriority: account.Preference.SchedulerPriority,
			Alias:             account.Preference.Alias,
			RefreshEnabled:    account.RefreshEnabled(),
			Disabled401:       account.Disabled401,
			Banned:            account.Banned(now),
			BanUntil:          account.BanUntil,
			Available:         account.Selectable(now, cfg.QuotaStaleAfter),
			UnavailableReason: coreAccountUnavailableReason(account, cfg, now),
			LastRefreshAt:     account.LastRefreshAt,
			LastSuccessAt:     account.LastSuccessAt,
			LastError:         account.LastError,
			FiveHour:          account.Quota.FiveHour,
			LongWindow:        account.Quota.LongWindow,
			ProbeDueAt:        account.ProbeDueAt,
			LastProbeAt:       account.LastProbeAt,
			ProbeStatus:       account.ProbeStatus,
		})
	}
	return out
}

func coreJSON(status int, payload any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(payload)
	if err != nil {
		status = http.StatusInternalServerError
		raw = []byte(`{"error":"encode failed"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":           []string{"application/json; charset=utf-8"},
			"Cache-Control":          []string{"no-store"},
			"X-Content-Type-Options": []string{"nosniff"},
		},
		Body: raw,
	}
}

const coreStatusHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Codex 调度器</title><style>
body{font-family:system-ui,-apple-system,sans-serif;margin:24px;max-width:1180px;color:#111;background:#fafafa}h1{margin-bottom:4px}.muted{color:#666}.bar,.card{background:#fff;border:1px solid #ddd;border-radius:10px;padding:14px;margin:12px 0}.bar{display:flex;gap:10px;align-items:center;flex-wrap:wrap}button,input{font:inherit;padding:7px 10px}button{cursor:pointer}.account{display:grid;grid-template-columns:minmax(220px,2fr) 90px 100px 120px 140px 1fr;gap:10px;align-items:center;border-top:1px solid #eee;padding:12px 0}.account:first-child{border-top:0}.bad{font-weight:600}.ok{font-weight:600}.switch{display:flex;gap:6px;align-items:center}.small{font-size:12px}.quota{white-space:nowrap}@media(max-width:800px){.account{grid-template-columns:1fr 1fr}.wide{grid-column:1/-1}}
</style></head><body><h1>Codex 调度器</h1><div class="muted">CPA priority 管跨层；插件只在同 priority 内调度。所有 CPA 启用的 Codex 账号都会显示，后台刷新默认开启。</div>
<div class="bar"><button id="refreshAll">刷新全部</button><label class="switch"><input id="resetProbe" type="checkbox">Reset Probe</label><label>到期后 <input id="probeDelay" value="5m" size="6"></label><button id="saveSettings">保存设置</button><span id="msg" class="small muted"></span></div>
<div id="accounts" class="card">加载中…</div>
<script>
const base='/v0/management/plugins/codex-quota-scheduler';
const q=s=>document.querySelector(s); const node=(tag,text)=>{const n=document.createElement(tag);if(text!==undefined)n.textContent=text;return n};
function pct(w){if(!w||w.UsedPercent===undefined&&w.used_percent===undefined)return '—';const v=w.used_percent??w.UsedPercent;return (100-v).toFixed(1)+'% 剩余'}
function when(v){if(!v||v.startsWith('0001-'))return '—';return new Date(v).toLocaleString()}
async function api(path,opt){const r=await fetch(base+path,{credentials:'same-origin',headers:{'Content-Type':'application/json'},...opt});if(!r.ok)throw new Error('HTTP '+r.status);return r.json()}
async function load(){try{const s=await api('/status');q('#resetProbe').checked=s.settings.enable_reset_probe;q('#probeDelay').value=s.settings.reset_probe_after_reset_delay;const root=q('#accounts');root.textContent='';for(const a of s.accounts){const row=node('div');row.className='account';const who=node('div');who.className='wide';who.append(node('div',a.alias||a.label||a.email||a.name||a.auth_id));who.append(node('div','ID '+a.auth_id+' · CPA priority '+a.cpa_priority));who.lastChild.className='small muted';row.append(who);const sp=document.createElement('input');sp.type='number';sp.value=a.scheduler_priority;sp.title='同 CPA priority 内的插件优先级';row.append(sp);const sw=node('label');sw.className='switch';const cb=document.createElement('input');cb.type='checkbox';cb.checked=a.refresh_enabled;sw.append(cb,node('span','刷新'));row.append(sw);const status=node('div',a.disabled_401?'401 禁用':a.banned?'429 Ban':a.available?'可用':a.unavailable_reason||'等待额度');status.className=a.available?'ok':'bad';row.append(status);const quota=node('div','5h '+pct(a.five_hour));quota.className='quota';row.append(quota);const detail=node('div','最后刷新 '+when(a.last_success_at)+' · Probe '+(a.probe_status||'—'));detail.className='small muted wide';row.append(detail);cb.addEventListener('change',()=>patch(a.auth_id,{refresh_enabled:cb.checked}));sp.addEventListener('change',()=>patch(a.auth_id,{scheduler_priority:Number(sp.value)||0}));root.append(row)}q('#msg').textContent='更新 '+new Date(s.generated_at).toLocaleTimeString()}catch(e){q('#msg').textContent='加载失败 '+e.message}}
async function patch(id,extra){try{await api('/account',{method:'PATCH',body:JSON.stringify({auth_id:id,...extra})});await load()}catch(e){q('#msg').textContent='保存失败 '+e.message}}
q('#refreshAll').onclick=async()=>{await api('/refresh',{method:'POST',body:'{}'});q('#msg').textContent='已请求刷新';setTimeout(load,1200)};
q('#saveSettings').onclick=async()=>{try{const cur=await api('/settings');cur.enable_reset_probe=q('#resetProbe').checked;cur.reset_probe_after_reset_delay=q('#probeDelay').value;await api('/settings',{method:'PUT',body:JSON.stringify(cur)});await load()}catch(e){q('#msg').textContent='设置失败 '+e.message}};
load();setInterval(load,30000);
</script></body></html>`
