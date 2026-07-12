package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jeffery/codex-quota-scheduler/testsupport"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type sequenceProbeHost struct {
	mu    sync.Mutex
	auth  pluginapi.HostAuthGetResponse
	quota [][]byte
	urls  []string
}

func TestProbeAuthBlockedResumesOnlyAfterExternalLoginEpoch(t *testing.T) { //inv:INV-23,INV-33
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	finger := NewCredentialFingerprint("acct", "r0", "idx")
	if _, err := store.Update(func(s *PersistentState) error {
		s.Bindings["a"] = RuntimeBinding{AuthID: "a", Instance: 1, Login: 4, Fingerprint: finger}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	registry, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.MarkAuthBlocked("a"); err != nil {
		t.Fatal(err)
	}
	if err = registry.ObserveExternalLogin("a", 4, finger); err != nil {
		t.Fatal(err)
	}
	b, _ := registry.Lookup("a")
	if !b.AuthBlocked {
		t.Fatal("same LoginEpoch unlocked AuthBlocked")
	}
	if err = registry.ObserveExternalLogin("a", 5, finger); err != nil {
		t.Fatal(err)
	}
	b, _ = registry.Lookup("a")
	if b.AuthBlocked || b.Login != 5 {
		t.Fatalf("binding=%#v", b)
	}
}

func (h *sequenceProbeHost) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	return []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: 9}}, nil
}
func (h *sequenceProbeHost) GetAuth(string) (pluginapi.HostAuthGetResponse, error) {
	return h.auth, nil
}
func (h *sequenceProbeHost) SaveAuth(string, json.RawMessage) error { return nil }
func (h *sequenceProbeHost) Log(string, string, map[string]any)     {}
func (h *sequenceProbeHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.urls = append(h.urls, req.URL)
	if req.URL == codexResetProbeEndpoint {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"total_tokens":1}}`)}, nil
	}
	body := h.quota[0]
	h.quota = h.quota[1:]
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
}

func TestProductionProbeRunsWhileNormalRefreshDormant(t *testing.T) { //inv:INV-07,INV-14,INV-17,INV-26,INV-32
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{[]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(-time.Hour).Format(time.RFC3339))), []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	instance := legacyAuthInstanceID("a")
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Instance: instance, TemporaryExhausted: true, TemporaryResetAt: now.Add(time.Hour), Circuit: CircuitBreakerState{State: CircuitStateOpen, FailureCount: 7}, Quota: ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: now.Add(-time.Hour), UsedPercent: ptrFloat(80), Exhausted: true}}})
	previousTrials := globalTrials
	globalTrials = NewTrialRegistry()
	globalTrials.TryBegin(instance, now)
	t.Cleanup(func() { globalTrials = previousTrials })
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if _, _, err = r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if r.refreshController.Mode(now) != RefreshModeDormant {
		t.Fatal("normal refresh not dormant")
	}
	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	urls := append([]string(nil), host.urls...)
	host.mu.Unlock()
	want := []string{cfg.QuotaEndpoint, codexResetProbeEndpoint, cfg.QuotaEndpoint}
	if len(urls) != len(want) {
		t.Fatalf("urls=%v", urls)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Fatalf("urls=%v", urls)
		}
	}
	w, ok := r.probeController.Window(legacyAuthInstanceID("a"), ProbeWindowFiveHour)
	if !ok || w.State != ProbeConfirmed {
		t.Fatalf("window=%#v ok=%v", w, ok)
	}
	a := accountByAuthID(t, state.Snapshot(now), "a")
	if a.Circuit.FailureCount != 7 || a.Circuit.State != CircuitStateOpen || globalTrials.State(instance, now) != TrialActive {
		t.Fatalf("probe mutated business state circuit=%#v trial=%v", a.Circuit, globalTrials.State(instance, now))
	}
}

func TestProductionProbeKPointCrashRestartVerifyFirst(t *testing.T) { //inv:INV-27,INV-36,INV-38
	points := []string{"K_PROBE_SENDING_WRITE", "K_PROBE_AFTER_SENDING", "K_PROBE_BEFORE_HTTP", "K_PROBE_AFTER_HTTP", "K_PROBE_SENT_WRITE"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			clock := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
			idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
			host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{[]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, clock.Add(-time.Hour).Format(time.RFC3339))), []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, clock.Add(4*time.Hour).Format(time.RFC3339)))}}
			cfg := DefaultConfig()
			cfg.EnableResetProbe = true
			pluginState := NewPluginState(cfg)
			pluginState.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
			pluginState.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Quota: ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: clock.Add(-time.Hour), UsedPercent: ptrFloat(80)}}})
			roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
			path := filepath.Join(t.TempDir(), "state.json")
			adapter := &rosterCredentialHost{host: host, roster: roster}
			r, err := NewProductionQuotaRefresher(host, pluginState, adapter, roster, path, func() time.Time { return clock })
			if err != nil {
				t.Fatal(err)
			}
			adapter.bindings = r.bindings
			if _, _, err = r.BootstrapBinding(context.Background(), "a"); err != nil {
				t.Fatal(err)
			}
			r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
			registry := testsupport.NewKPointRegistry(points...)
			r.probeWAL.crash = testsupport.NewCrashController(registry, point)
			if err = r.RunProbeDueOnce(context.Background()); !errors.Is(err, testsupport.ErrInjectedCrash) {
				t.Fatalf("err=%v", err)
			}
			clock = clock.Add(4 * time.Second)
			adapter2 := &rosterCredentialHost{host: host, roster: roster}
			restart, err := NewProductionQuotaRefresher(host, pluginState, adapter2, roster, path, func() time.Time { return clock })
			if err != nil {
				t.Fatal(err)
			}
			adapter2.bindings = restart.bindings
			restart.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
			if point != "K_PROBE_SENDING_WRITE" {
				if err = restart.RunProbeRecoveryOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			host.mu.Lock()
			posts := 0
			for _, u := range host.urls {
				if u == codexResetProbeEndpoint {
					posts++
				}
			}
			host.mu.Unlock()
			if posts > 1 {
				t.Fatalf("probe resent after crash: urls=%v", host.urls)
			}
		})
	}
}
