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
	if len(resp.Resources) != 1 || resp.Resources[0].Path != "/status" {
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
		"POST /plugins/codex-quota-scheduler/refresh",
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
	previousRefreshSoon := managementRefreshSoon
	managementRefreshSoon = func() { refreshes++ }
	t.Cleanup(func() { managementRefreshSoon = previousRefreshSoon })

	tests := []struct {
		name   string
		method string
		path   string
		query  url.Values
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
			query:  url.Values{"format": []string{"json"}},
			want:   http.StatusOK,
		},
		{
			name:   "management annotations",
			method: http.MethodGet,
			path:   "/v0/management/plugins/codex-quota-scheduler/annotations",
			want:   http.StatusOK,
		},
		{
			name:   "resource annotations",
			method: http.MethodGet,
			path:   "/v0/resource/plugins/codex-quota-scheduler/annotations",
			want:   http.StatusOK,
		},
		{
			name:   "management refresh",
			method: http.MethodPost,
			path:   "/v0/management/plugins/codex-quota-scheduler/refresh",
			want:   http.StatusAccepted,
		},
		{
			name:   "resource refresh",
			method: http.MethodPost,
			path:   "/v0/resource/plugins/codex-quota-scheduler/refresh",
			want:   http.StatusAccepted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := HandleManagementRequest(store, pluginapi.ManagementRequest{
				Method: tt.method,
				Path:   tt.path,
				Query:  tt.query,
			}, now)
			if resp.StatusCode != tt.want {
				t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, tt.want, resp.Body)
			}
		})
	}
	if refreshes != 2 {
		t.Fatalf("refreshes = %d, want 2", refreshes)
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
		t.Fatalf("StatusCode = %d, want %d; body=%s", resp.StatusCode, http.StatusOK, resp.Body)
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
	for _, forbidden := range []string{"access_token", "bearer ", "authorization", "cookie"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("html contains sensitive field %q: %s", forbidden, html)
		}
	}
	if strings.Contains(html, "<script>alert") || !strings.Contains(html, "&lt;script&gt;") || !strings.Contains(html, "Ops &amp; Finance") {
		t.Fatalf("html did not escape user fields: %s", html)
	}
}

func TestAnnotationsEndpointsNormalizePatchAndPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "annotations.json")
	cfg := DefaultConfig()
	cfg.AnnotationStatePath = path
	store := NewPluginState(cfg)
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
	raw, err := os.ReadFile(path)
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
			cfg := DefaultConfig()
			cfg.AnnotationStatePath = filepath.Join(t.TempDir(), "annotations.json") + string(rune(0))
			store := NewPluginState(cfg)
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
