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
	Accounts      []StatusAccount `json:"accounts"`
}

type StatusAccount struct {
	Rank              int           `json:"rank"`
	AuthID            string        `json:"auth_id"`
	Alias             string        `json:"alias,omitempty"`
	GroupID           string        `json:"group_id,omitempty"`
	Group             string        `json:"group,omitempty"`
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

		status := StatusAccount{
			Rank:              i + 1,
			AuthID:            scheduled.AuthID,
			Alias:             scheduled.Annotation.Alias,
			GroupID:           groupID,
			Group:             groupName,
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
	cfg := store.Config()
	if cfg.AnnotationStatePath == "" {
		return nil
	}
	return SaveAnnotations(cfg.AnnotationStatePath, state)
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
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.PluginID}} status</title>
<style>
body{font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:24px;color:#202124;background:#fff}
h1{font-size:20px;margin:0 0 12px}
.summary{display:flex;flex-wrap:wrap;gap:8px 18px;margin:0 0 16px;font-size:13px;color:#3c4043}
table{border-collapse:collapse;width:100%;font-size:13px}
th,td{border-bottom:1px solid #dadce0;padding:7px 8px;text-align:left;vertical-align:top}
th{background:#f8fafd;font-weight:600;color:#3c4043}
.yes{color:#137333;font-weight:600}.no{color:#b3261e;font-weight:600}
code{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:12px}
</style>
</head>
<body>
<h1>{{.PluginID}}</h1>
<div class="summary">
<span>Next: <code>{{if .NextAuthID}}{{.NextAuthID}}{{else}}none{{end}}</code></span>
<span>Monthly mode: {{.MonthlyMode}}</span>
<span>Handle: {{if .HandleEnabled}}enabled{{else}}disabled{{end}}</span>
<span>Last selected: <code>{{if .LastSelected}}{{.LastSelected}}{{else}}none{{end}}</code></span>
<span>Last reason: {{if .LastReason}}{{.LastReason}}{{else}}none{{end}}</span>
</div>
<table>
<thead><tr><th>Rank</th><th>Auth ID</th><th>Alias</th><th>Group</th><th>Tags</th><th>CPA priority</th><th>Family</th><th>Availability</th><th>Reset/expiry</th><th>Cache age</th></tr></thead>
<tbody>
{{range .Accounts}}
<tr>
<td>{{.Rank}}</td>
<td><code>{{.AuthID}}</code></td>
<td>{{.Alias}}</td>
<td>{{.Group}}</td>
<td>{{join .Tags ", "}}</td>
<td>{{.CPAPriority}}</td>
<td>{{.Family}}</td>
<td>{{if .Available}}<span class="yes">available</span>{{else}}<span class="no">{{.UnavailableReason}}</span>{{end}}</td>
<td>{{.ResetExpiryText}}</td>
<td>{{.CacheAge}}</td>
</tr>
{{else}}
<tr><td colspan="10">No accounts</td></tr>
{{end}}
</tbody>
</table>
</body>
</html>`))
