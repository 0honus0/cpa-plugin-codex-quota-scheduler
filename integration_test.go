package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func cleanupIntegrationGlobals(t *testing.T) {
	t.Helper()

	previousState := globalState
	previousConfig, hadConfig := currentConfig.Load().(Config)
	refresherMu.Lock()
	previousRefresher := globalRefresher
	globalRefresher = nil
	refresherMu.Unlock()
	previousRefreshSoon := managementRefreshSoon
	managementRefreshSoon = func() {}
	currentConfig = atomic.Value{}

	t.Cleanup(func() {
		refresherMu.Lock()
		testRefresher := globalRefresher
		if testRefresher != nil && testRefresher != previousRefresher {
			testRefresher.Stop()
		}
		globalRefresher = previousRefresher
		refresherMu.Unlock()

		globalState = previousState
		currentConfig = atomic.Value{}
		if hadConfig {
			currentConfig.Store(previousConfig)
		}
		managementRefreshSoon = previousRefreshSoon
	})
}

func TestHandleMethodRegisterEnvelope(t *testing.T) {
	cleanupIntegrationGlobals(t)

	raw, err := handleMethod(pluginabi.MethodPluginRegister, []byte(`{"config_yaml":"aGFuZGxlX2VuYWJsZWQ6IHRydWUK"}`))
	if err != nil {
		t.Fatalf("handle register: %v", err)
	}

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got %+v", env)
	}
}

func TestHandleMethodRegisterPreservesConfigWhenStateFileMissing(t *testing.T) {
	cleanupIntegrationGlobals(t)

	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "missing-state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	rawReq, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("handle_enabled: false\nmonthly_mode: priority\nquota_refresh_interval: 45s\nmax_refresh_concurrency: 2\n")})
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if _, err := handleMethod(pluginabi.MethodPluginRegister, rawReq); err != nil {
		t.Fatalf("handle register: %v", err)
	}

	cfg := globalState.Config()
	if cfg.HandleEnabled || cfg.MonthlyMode != MonthlyModePriority || cfg.QuotaRefreshInterval.String() != "45s" || cfg.MaxRefreshConcurrency != 2 {
		t.Fatalf("config = %#v, want lifecycle config preserved", cfg)
	}
}

func TestHandleMethodRegisterLoadsPersistedAnnotations(t *testing.T) {
	cleanupIntegrationGlobals(t)

	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	if err := SavePluginDiskState(defaultStatePath(), PluginDiskState{
		Config: DefaultConfig(),
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "Personal", GroupID: "team"},
		},
		Groups: map[string]GroupAnnotation{
			"team": {Name: "Team"},
		},
	}); err != nil {
		t.Fatalf("SavePluginDiskState returned error: %v", err)
	}

	rawReq, err := json.Marshal(lifecycleRequest{})
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if _, err := handleMethod(pluginabi.MethodPluginRegister, rawReq); err != nil {
		t.Fatalf("handle register: %v", err)
	}

	state := globalState.Annotations()
	if state.Accounts["auth:auth-1"].Alias != "Personal" {
		t.Fatalf("account annotations = %#v, want persisted alias", state.Accounts)
	}
	if state.Groups["team"].Name != "Team" {
		t.Fatalf("group annotations = %#v, want persisted group", state.Groups)
	}
}

func TestHandleMethodRegisterIgnoresInvalidPersistedAnnotations(t *testing.T) {
	cleanupIntegrationGlobals(t)

	dir := t.TempDir()
	previousDefaultStatePath := defaultStatePath
	defaultStatePath = func() string { return filepath.Join(dir, "state.json") }
	t.Cleanup(func() { defaultStatePath = previousDefaultStatePath })

	if err := os.WriteFile(defaultStatePath(), []byte("{not-json"), 0600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	rawReq, err := json.Marshal(lifecycleRequest{})
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	raw, err := handleMethod(pluginabi.MethodPluginRegister, rawReq)
	if err != nil {
		t.Fatalf("handle register: %v", err)
	}

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got %+v", env)
	}
}

func TestHandleMethodSchedulerPickFallbackEnvelope(t *testing.T) {
	cleanupIntegrationGlobals(t)

	globalState = NewPluginState(DefaultConfig())
	rawReq, err := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "missing", Provider: "codex", Priority: 1},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	raw, err := handleMethod(pluginabi.MethodSchedulerPick, rawReq)
	if err != nil {
		t.Fatalf("handle scheduler pick: %v", err)
	}

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got %+v", env)
	}

	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode scheduler response: %v", err)
	}
	if !resp.Handled || resp.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("expected fill-first fallback, got %+v", resp)
	}
}
