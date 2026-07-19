package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jeffery/codex-quota-scheduler/testsupport"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type sequenceProbeHost struct {
	mu            sync.Mutex
	auth          pluginapi.HostAuthGetResponse
	authByIndex   map[string]pluginapi.HostAuthGetResponse
	authFiles     []pluginapi.HostAuthFileEntry
	authReads     []pluginapi.HostAuthGetResponse
	quota         [][]byte
	urls          []string
	requests      []pluginapi.HTTPRequest
	quotaStatus   int
	gets          int
	gateAuthIndex string
	getStarted    chan struct{}
	releaseGet    chan struct{}
	doStarted     chan struct{}
	releaseDo     chan struct{}
}

func newProbeFixtureHost() *sequenceProbeHost {
	return &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{
		AuthIndex: "idx",
		Name:      "a.json",
		JSON:      json.RawMessage(`{"access_token":"access","refresh_token":"refresh","account_id":"acct"}`),
	}}
}

func TestProbeAuthBlockedResumesOnlyAfterExternalLoginEpoch(t *testing.T) {
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
	if len(h.authFiles) > 0 {
		return append([]pluginapi.HostAuthFileEntry(nil), h.authFiles...), nil
	}
	return []pluginapi.HostAuthFileEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: 9}}, nil
}
func (h *sequenceProbeHost) GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	h.mu.Lock()
	h.gets++
	auth, started, release := h.auth, h.getStarted, h.releaseGet
	if indexed, ok := h.authByIndex[authIndex]; ok {
		auth = indexed
	}
	if h.gateAuthIndex != "" && h.gateAuthIndex != authIndex {
		started, release = nil, nil
	}
	if len(h.authReads) > 0 {
		auth = h.authReads[0]
		h.authReads = h.authReads[1:]
	}
	h.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if release != nil {
		<-release
	}
	return auth, nil
}
func (h *sequenceProbeHost) SaveAuth(string, json.RawMessage) error { return nil }
func (h *sequenceProbeHost) Log(string, string, map[string]any)     {}
func (h *sequenceProbeHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.mu.Lock()
	started, release := h.doStarted, h.releaseDo
	if req.URL != codexResetProbeEndpoint {
		started, release = nil, nil
	}
	h.urls = append(h.urls, req.URL)
	h.requests = append(h.requests, req)
	h.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if release != nil {
		<-release
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.URL == codexResetProbeEndpoint {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"total_tokens":1}}`)}, nil
	}
	if req.URL == resetCreditsEndpoint {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	}
	body := h.quota[0]
	h.quota = h.quota[1:]
	status := h.quotaStatus
	if status == 0 {
		status = http.StatusOK
	}
	return pluginapi.HTTPResponse{StatusCode: status, Body: body}, nil
}

func newDueProbeRuntime(t *testing.T, now time.Time, host *sequenceProbeHost) *QuotaRefresher {
	t.Helper()
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	binding, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Deadline: now, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 80, fiveHourSeconds*time.Second)})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	refresherMu.Lock()
	previous := globalRosterController
	globalRosterController = nil
	refresherMu.Unlock()
	t.Cleanup(func() { refresherMu.Lock(); globalRosterController = previous; refresherMu.Unlock() })
	return r
}

func newProductionLazyRefreshRuntime(t *testing.T, now time.Time, host *sequenceProbeHost) *QuotaRefresher {
	t.Helper()
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if _, _, err := r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	refresherMu.Lock()
	previous := globalRosterController
	globalRosterController = nil
	refresherMu.Unlock()
	t.Cleanup(func() { refresherMu.Lock(); globalRosterController = previous; refresherMu.Unlock() })
	return r
}

func TestProductionRefreshLaunchesFirstObservedLazyProbe(t *testing.T) {
	now := time.Date(2026, 7, 19, 11, 18, 49, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{lazy, lazy, active}
	host.doStarted = make(chan struct{})
	r := newProductionLazyRefreshRuntime(t, now, host)
	t.Cleanup(r.Stop)

	if err := r.RefreshOneAuthID("a"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.doStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("production refresh did not launch Probe")
	}
	deadline := time.After(5 * time.Second)
	for {
		posts, _ := probePOSTCount(host)
		if posts == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("probe POST count = %d, want 1", posts)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestProbeRefreshRemovesAbsentFiveHourWindow(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.SetWindow(binding.Instance, ProbeWindowLong, ProbeWindow{State: ProbeWaitingReset, Deadline: now.Add(24 * time.Hour)})
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}

	quota := ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(24 * time.Hour)}}
	if err := r.reconcileObservedProbeWindows(binding.Instance, quota); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour); ok {
		t.Fatal("absent FiveHour Probe window retained")
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowLong); !ok {
		t.Fatal("LongWindow Probe state removed")
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; ok {
		t.Fatal("persisted absent FiveHour Probe window retained")
	}
	if _, ok := persisted.ProbeWindows[binding.Instance][ProbeWindowLong]; !ok {
		t.Fatal("persisted LongWindow Probe state removed")
	}
}

func TestProbeRefreshPreservesAbsentFiveHourDuringNonterminalAttempt(t *testing.T) {
	for _, phase := range []ProbeAttemptPhase{ProbeAttemptPrepared, ProbeAttemptSending, ProbeAttemptSent, ProbeAttemptSentUnknown} {
		t.Run(string(phase), func(t *testing.T) {
			now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
			r := newDueProbeRuntime(t, now, newProbeFixtureHost())
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			if _, err := r.runtimeStore.Update(func(s *PersistentState) error {
				s.ProbeWindows = r.probeController.Snapshot()
				s.ProbeAttempts[binding.Instance] = ProbeAttempt{Instance: binding.Instance, AttemptID: "active", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: phase}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			quota := ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(24 * time.Hour)}}
			if err := r.reconcileObservedProbeWindows(binding.Instance, quota); err != nil {
				t.Fatal(err)
			}
			if _, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour); !ok {
				t.Fatal("nonterminal attempt lost referenced FiveHour state")
			}
			persisted, err := r.runtimeStore.PersistentSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; !ok {
				t.Fatal("nonterminal attempt lost persisted FiveHour state")
			}
		})
	}
}

func TestProbeBootstrapRecreatesReappearedFiveHour(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	r := newDueProbeRuntime(t, now, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowFiveHour)
	if err := r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	fiveUsed := 20.0
	fiveReset := now.Add(5 * time.Hour)
	r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: now, Quota: ParsedQuota{
		Family:     AccountFamilyWeekly,
		FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &fiveUsed, ResetAt: fiveReset},
		LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(7 * 24 * time.Hour)},
	}})

	if err := r.bootstrapProbeWindows(); err != nil {
		t.Fatal(err)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour)
	if !ok || !window.Baseline.ResetAt.Equal(fiveReset) {
		t.Fatalf("recreated FiveHour window = %#v, ok=%v", window, ok)
	}
}

func TestProbeBootstrapSchedulesFirstObservationLazyWindowsImmediately(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	fiveSeconds := int64(5 * time.Hour / time.Second)
	weekSeconds := int64(7 * 24 * time.Hour / time.Second)
	monthSeconds := int64(30 * 24 * time.Hour / time.Second)
	zero := 0.0

	tests := []struct {
		name   string
		kind   ProbeWindowKind
		family AccountFamily
		window QuotaWindow
	}{
		{"five-hour", ProbeWindowFiveHour, AccountFamilyWeekly, QuotaWindow{Kind: WindowFiveHour, UsedPercent: &zero, LimitWindowSeconds: &fiveSeconds, ResetAt: now.Add(5 * time.Hour)}},
		{"weekly", ProbeWindowLong, AccountFamilyWeekly, QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7 * 24 * time.Hour)}},
		{"monthly", ProbeWindowLong, AccountFamilyMonthly, QuotaWindow{Kind: WindowMonthly, UsedPercent: &zero, LimitWindowSeconds: &monthSeconds, ResetAt: now.Add(30 * 24 * time.Hour)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newDueProbeRuntime(t, now, newProbeFixtureHost())
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			r.probeController.RemoveWindow(binding.Instance, tt.kind)
			quota := ParsedQuota{Family: tt.family, LongWindow: &tt.window}
			if tt.kind == ProbeWindowFiveHour {
				quota.FiveHour = &tt.window
				quota.LongWindow = &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(24 * time.Hour)}
			}
			r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: quota.Family, LastSuccessAt: now, Quota: quota})

			if err := r.bootstrapProbeWindows(); err != nil {
				t.Fatal(err)
			}
			window, ok := r.probeController.Window(binding.Instance, tt.kind)
			if !ok || window.State != ProbePendingCheck || !window.Baseline.SuspectedLazy || window.Baseline.WindowLength <= 0 {
				t.Fatalf("window = %#v, ok=%v", window, ok)
			}
		})
	}
}

func TestProbeBootstrapRejectsFirstObservationLazyFalsePositives(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	zero, used := 0.0, 1.0
	weekSeconds := int64(7 * 24 * time.Hour / time.Second)

	tests := []struct {
		name   string
		family AccountFamily
		window QuotaWindow
	}{
		{"non-zero-usage", AccountFamilyWeekly, QuotaWindow{Kind: WindowWeekly, UsedPercent: &used, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7 * 24 * time.Hour)}},
		{"missing-usage", AccountFamilyWeekly, QuotaWindow{Kind: WindowWeekly, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7 * 24 * time.Hour)}},
		{"monthly-duration-unknown", AccountFamilyMonthly, QuotaWindow{Kind: WindowMonthly, UsedPercent: &zero, ResetAt: now.Add(30 * 24 * time.Hour)}},
		{"anchor-outside-tolerance", AccountFamilyWeekly, QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &weekSeconds, ResetAt: now.Add(7*24*time.Hour + 10*time.Minute)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newDueProbeRuntime(t, now, newProbeFixtureHost())
			binding, ok := r.bindings.Lookup("a")
			if !ok {
				t.Fatal("binding missing")
			}
			r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
			r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: tt.family, LastSuccessAt: now, Quota: ParsedQuota{Family: tt.family, LongWindow: &tt.window}})
			if err := r.bootstrapProbeWindows(); err != nil {
				t.Fatal(err)
			}
			window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
			if !ok || window.State != ProbeWaitingReset || window.Baseline.SuspectedLazy {
				t.Fatalf("window = %#v, ok=%v", window, ok)
			}
		})
	}
}

func TestProbeBootstrapUsesQuotaObservationTimeForLazyAnchor(t *testing.T) {
	observedAt := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	bootstrapAt := observedAt.Add(10 * time.Minute)
	r := newDueProbeRuntime(t, bootstrapAt, newProbeFixtureHost())
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
	zero := 0.0
	seconds := int64(604800)
	r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: observedAt, Quota: ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: observedAt.Add(7 * 24 * time.Hour)}}})

	if err := r.bootstrapProbeWindows(); err != nil {
		t.Fatal(err)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
	if !ok || window.State != ProbePendingCheck || !window.Baseline.SuspectedLazy {
		t.Fatalf("window = %#v, ok=%v", window, ok)
	}
}

func TestSuspectedLazyProbeBaselinePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runtime-state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	base := ResetProbeBaseline(now.Add(7*24*time.Hour), 0, 7*24*time.Hour)
	base.SuspectedLazy = true
	if _, err := store.Update(func(state *PersistentState) error {
		state.ProbeWindows[1] = map[ProbeWindowKind]ProbeWindow{
			ProbeWindowLong: {State: ProbePendingCheck, Baseline: base},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	restarted := NewStateStore(path, OSFileHooks(), nil)
	persisted, err := restarted.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	window := persisted.ProbeWindows[1][ProbeWindowLong]
	if !window.Baseline.SuspectedLazy || window.State != ProbePendingCheck {
		t.Fatalf("persisted window = %#v", window)
	}
}

func newFirstObservedWeeklyLazyRuntime(t *testing.T, now time.Time) (*QuotaRefresher, *sequenceProbeHost, RuntimeBinding) {
	t.Helper()
	reset := now.Add(7 * 24 * time.Hour)
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.quota = [][]byte{lazy, active}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowFiveHour)
	r.probeController.RemoveWindow(binding.Instance, ProbeWindowLong)
	zero := 0.0
	seconds := int64(604800)
	r.state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Provider: "codex", Family: AccountFamilyWeekly, LastSuccessAt: now, Quota: ParsedQuota{Family: AccountFamilyWeekly, LongWindow: &QuotaWindow{Kind: WindowWeekly, UsedPercent: &zero, LimitWindowSeconds: &seconds, ResetAt: reset}}})
	if err := r.bootstrapProbeWindows(); err != nil {
		t.Fatal(err)
	}
	return r, host, binding
}

func probePOSTCount(host *sequenceProbeHost) (int, []string) {
	host.mu.Lock()
	defer host.mu.Unlock()
	posts := 0
	urls := append([]string(nil), host.urls...)
	for _, url := range urls {
		if url == codexResetProbeEndpoint {
			posts++
		}
	}
	return posts, urls
}

func TestFirstObservedWeeklyLazyWindowSendsOneActivationAndVerifies(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, binding := newFirstObservedWeeklyLazyRuntime(t, now)

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	posts, urls := probePOSTCount(host)
	if posts != 1 {
		t.Fatalf("probe POST count = %d, want 1; urls=%v", posts, urls)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
	if !ok || window.State != ProbeConfirmed || window.Baseline.SuspectedLazy {
		t.Fatalf("window = %#v, ok=%v", window, ok)
	}
	logs := r.state.Snapshot(now).Logs
	events := make([]string, 0, len(logs))
	for _, entry := range logs {
		if strings.HasPrefix(entry.Event, "probe.") {
			events = append(events, entry.Event)
		}
	}
	want := []string{"probe.precheck_started", "probe.activation_sent", "probe.verified"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("Probe events = %v, want %v", events, want)
	}
}

func TestProbeFailureLogRedactsSecrets(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, _ := newFirstObservedWeeklyLazyRuntime(t, now)
	host.mu.Lock()
	host.quotaStatus = http.StatusServiceUnavailable
	host.quota = [][]byte{[]byte(`{"access_token":"access-token-sentinel","refresh_token":"refresh-token-sentinel","account_id":"account-id-sentinel","authorization":"Bearer authorization-header-sentinel","request_body":"request-body-sentinel","response_body":"response-body-sentinel"}`)}
	host.mu.Unlock()

	if err := r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("RunProbeDueOnce returned nil error")
	}
	logs := r.state.Snapshot(now).Logs
	var failures []LogEntry
	for _, entry := range logs {
		if entry.Event == "probe.failed" {
			failures = append(failures, entry)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("probe.failed logs = %#v, want one terminal failure", failures)
	}
	if sent, ok := failures[0].Fields["sent"].(bool); !ok || sent {
		t.Fatalf("probe.failed sent = %#v, want false", failures[0].Fields["sent"])
	}
	if windows, ok := failures[0].Fields["windows"].([]ProbeWindowKind); !ok || !reflect.DeepEqual(windows, []ProbeWindowKind{ProbeWindowLong}) {
		t.Fatalf("probe.failed windows = %#v, want [long]", failures[0].Fields["windows"])
	}
	errText, _ := failures[0].Fields["error"].(string)
	for _, forbidden := range []string{"access-token-sentinel", "refresh-token-sentinel", "account-id-sentinel", "authorization-header-sentinel", "request-body-sentinel", "response-body-sentinel"} {
		if strings.Contains(errText, forbidden) {
			t.Fatalf("probe failure log leaked %q: %#v", forbidden, failures[0])
		}
	}
}

type probeTerminalWriteFailure struct {
	before   PersistentState
	injected bool
}

func failProbeTerminalWrite(t *testing.T, r *QuotaRefresher, instance AuthInstanceID, kind ProbeWindowKind, final ProbeWindowState) *probeTerminalWriteFailure {
	t.Helper()
	result := &probeTerminalWriteFailure{}
	before, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before.ProbeAttempts[instance]; ok {
		result.before = before
	}
	previous := r.runtimeStore.hooks.Replace
	r.runtimeStore.hooks.Replace = func(source, target string) error {
		raw, readErr := os.ReadFile(source)
		if readErr == nil {
			var candidate PersistentState
			if json.Unmarshal(raw, &candidate) == nil {
				if !result.injected {
					if _, ok := candidate.ProbeAttempts[instance]; ok {
						result.before = candidate
					}
					window, hasWindow := candidate.ProbeWindows[instance][kind]
					_, hasAttempt := candidate.ProbeAttempts[instance]
					if target == r.runtimeStore.path && hasWindow && window.State == final && !hasAttempt {
						result.injected = true
						return errors.New("terminal Probe persistence failed")
					}
				}
			}
		}
		if previous != nil {
			return previous(source, target)
		}
		return nil
	}
	t.Cleanup(func() { r.runtimeStore.hooks.Replace = previous })
	return result
}

func assertProbeTerminalWriteRolledBack(t *testing.T, r *QuotaRefresher, failure *probeTerminalWriteFailure, instance AuthInstanceID) PersistentState {
	t.Helper()
	if !failure.injected {
		t.Fatal("terminal Probe write failure was not injected")
	}
	restarted := NewStateStore(r.runtimeStore.path, OSFileHooks(), nil)
	persisted, err := restarted.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	wantAttempt, ok := failure.before.ProbeAttempts[instance]
	if !ok {
		t.Fatalf("failure injector did not observe a pre-terminal attempt: %#v", failure.before.ProbeAttempts)
	}
	got, ok := persisted.ProbeAttempts[instance]
	phaseOK := got.Phase == wantAttempt.Phase || (wantAttempt.Phase == ProbeAttemptSent && got.Phase == ProbeAttemptSentUnknown)
	normalized := got
	normalized.Phase = wantAttempt.Phase
	if !ok || !phaseOK || !reflect.DeepEqual(normalized, wantAttempt) {
		t.Fatalf("attempt after failed terminal write = %#v, ok=%v; want unchanged %#v", got, ok, wantAttempt)
	}
	if got, want := persisted.ProbeWindows[instance], failure.before.ProbeWindows[instance]; !reflect.DeepEqual(got, want) {
		t.Fatalf("windows after failed terminal write = %#v; want unchanged %#v", got, want)
	}
	return persisted
}

func assertProbeTerminalFailure(t *testing.T, r *QuotaRefresher, now time.Time, sent bool) {
	t.Helper()
	logs := r.state.Snapshot(now).Logs
	var failures []LogEntry
	for _, entry := range logs {
		if entry.Event == "probe.failed" {
			failures = append(failures, entry)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("probe.failed logs = %#v, want one terminal failure", failures)
	}
	if got, ok := failures[0].Fields["sent"].(bool); !ok || got != sent {
		t.Fatalf("probe.failed sent = %#v, want %t", failures[0].Fields["sent"], sent)
	}
	if logs[len(logs)-1].Event != "probe.failed" {
		t.Fatalf("last probe lifecycle log = %#v, want probe.failed", logs[len(logs)-1])
	}
}

func TestProbePostCompletionFailuresLogTerminalFailure(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	t.Run("recovery reconciliation", func(t *testing.T) {
		r, host, binding := newFirstObservedWeeklyLazyRuntime(t, now)
		host.mu.Lock()
		host.quota = [][]byte{append([]byte(nil), host.quota[1]...)}
		host.mu.Unlock()
		if _, err := r.probeFence.Next(); err != nil {
			t.Fatal(err)
		}
		attempt := ProbeAttempt{Instance: binding.Instance, AttemptID: "recovery-attempt", Windows: []ProbeWindowKind{ProbeWindowLong}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: 1, VerifyNotBefore: now, SuppressUntil: now.Add(10 * time.Minute)}
		window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
		if !ok {
			t.Fatal("long window missing")
		}
		window.State = ProbeSentUnknown
		window.AttemptID = attempt.AttemptID
		r.probeController.SetWindow(binding.Instance, ProbeWindowLong, window)
		if err := r.persistProbeWindows(); err != nil {
			t.Fatal(err)
		}
		if _, err := r.runtimeStore.Update(func(state *PersistentState) error {
			state.ProbeAttempts[binding.Instance] = attempt
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		failure := failProbeTerminalWrite(t, r, binding.Instance, ProbeWindowLong, ProbeConfirmed)
		result := r.runTypedHeld(context.Background(), Intent{AuthID: "a", Instance: binding.Instance, Class: OperationProbeSequence, Source: SourceProbeVerify, StartedAfter: attempt.SendFenceSeq, AttemptID: attempt.AttemptID, Payload: probeSequencePayload{Binding: binding, Windows: attempt.Windows, Attempt: attempt, Recovery: true}}, &HeldLease{coordinator: r.coordinator})
		if result.Err == nil {
			t.Fatal("recovery returned nil error")
		}
		assertProbeTerminalWriteRolledBack(t, r, failure, binding.Instance)
		assertProbeTerminalFailure(t, r, now, true)
	})

	t.Run("precheck reconciliation", func(t *testing.T) {
		r, host, _ := newFirstObservedWeeklyLazyRuntime(t, now)
		host.mu.Lock()
		host.quota = [][]byte{append([]byte(nil), host.quota[1]...)}
		host.mu.Unlock()
		binding, ok := r.bindings.Lookup("a")
		if !ok {
			t.Fatal("binding missing")
		}
		failure := failProbeTerminalWrite(t, r, binding.Instance, ProbeWindowLong, ProbeConfirmed)
		if err := r.RunProbeDueOnce(context.Background()); err == nil {
			t.Fatal("precheck returned nil error")
		}
		assertProbeTerminalWriteRolledBack(t, r, failure, binding.Instance)
		assertProbeTerminalFailure(t, r, now, false)
	})

	t.Run("verify reconciliation", func(t *testing.T) {
		r, _, binding := newFirstObservedWeeklyLazyRuntime(t, now)
		failure := failProbeTerminalWrite(t, r, binding.Instance, ProbeWindowLong, ProbeConfirmed)
		if err := r.RunProbeDueOnce(context.Background()); err == nil {
			t.Fatal("verify returned nil error")
		}
		assertProbeTerminalWriteRolledBack(t, r, failure, binding.Instance)
		assertProbeTerminalFailure(t, r, now, true)
	})
}

func TestProbeTerminalCompletionFailureRestartsVerifyFirst(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, binding := newFirstObservedWeeklyLazyRuntime(t, now)
	host.mu.Lock()
	host.quota = append(host.quota, append([]byte(nil), host.quota[1]...))
	host.mu.Unlock()
	failure := failProbeTerminalWrite(t, r, binding.Instance, ProbeWindowLong, ProbeConfirmed)

	if err := r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("terminal persistence failure was not surfaced")
	}
	persisted := assertProbeTerminalWriteRolledBack(t, r, failure, binding.Instance)
	attempt := persisted.ProbeAttempts[binding.Instance]
	if attempt.AttemptID == "" || attempt.SendFenceSeq == 0 || (attempt.Phase != ProbeAttemptSent && attempt.Phase != ProbeAttemptSentUnknown) {
		t.Fatalf("failed completion did not retain recovery attempt: %#v", attempt)
	}
	postsBefore, urlsBefore := probePOSTCount(host)
	if postsBefore != 1 {
		t.Fatalf("probe POST count before restart = %d, want 1; urls=%v", postsBefore, urlsBefore)
	}

	restartNow := now.Add(4 * time.Second)
	roster := r.runtimeRoster()
	adapter := &rosterCredentialHost{host: host, roster: roster}
	restart, err := NewProductionQuotaRefresher(host, r.state, adapter, roster, r.runtimeStore.path, func() time.Time { return restartNow })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = restart.bindings
	restart.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if err = restart.RunProbeRecoveryOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	postsAfter, urlsAfter := probePOSTCount(host)
	if postsAfter != 1 {
		t.Fatalf("Probe recovery resent activation: posts=%d urls=%v", postsAfter, urlsAfter)
	}
	completed, err := restart.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := completed.ProbeAttempts[binding.Instance]; ok {
		t.Fatalf("recovery attempt survived successful verification: %#v", completed.ProbeAttempts[binding.Instance])
	}
	if window := completed.ProbeWindows[binding.Instance][ProbeWindowLong]; window.State != ProbeConfirmed {
		t.Fatalf("recovery window = %#v, want Confirmed", window)
	}
}

func TestProbeTerminalCompletionMergesOnlyCompletingInstance(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	attempt := ProbeAttempt{Instance: 1, AttemptID: "a-terminal", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSent, SendFenceSeq: 7, VerifyNotBefore: now}
	bPending := ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(now.Add(7*24*time.Hour), 0, 7*24*time.Hour)}
	if _, err := store.Update(func(state *PersistentState) error {
		state.ProbeAttempts[1] = attempt
		state.ProbeWindows[1] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: {State: ProbeSentAwaitingVerify}}
		state.ProbeWindows[2] = map[ProbeWindowKind]ProbeWindow{ProbeWindowLong: bPending}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	controller := NewProbeController(now)
	controller.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbeConfirmed})
	r := &QuotaRefresher{runtimeStore: store, probeController: controller}
	quota := ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, ResetAt: now.Add(5 * time.Hour)}}

	if err := r.persistTerminalProbeCompletion(1, attempt.AttemptID, quota); err != nil {
		t.Fatal(err)
	}
	persisted, err := NewStateStore(r.runtimeStore.path, OSFileHooks(), nil).PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := persisted.ProbeWindows[2][ProbeWindowLong]; !ok || !reflect.DeepEqual(got, bPending) {
		t.Fatalf("instance B window overwritten by A completion: got=%#v ok=%v want=%#v", got, ok, bPending)
	}
	if _, ok := persisted.ProbeAttempts[1]; ok {
		t.Fatalf("instance A attempt survived completion: %#v", persisted.ProbeAttempts[1])
	}
}

func probePOSTCountsByAccount(host *sequenceProbeHost) map[string]int {
	host.mu.Lock()
	defer host.mu.Unlock()
	counts := map[string]int{}
	for _, req := range host.requests {
		if req.URL == codexResetProbeEndpoint {
			counts[req.Headers.Get("Chatgpt-Account-Id")]++
		}
	}
	return counts
}

func TestProbeRefreshDuringActiveRunCoalescesRerunAndPersistsSecondInstance(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 30, 0, 0, time.UTC)
	aLazyReset := now.Add(-time.Hour)
	aActiveReset := now.Add(4 * time.Hour)
	bReset := now.Add(7 * 24 * time.Hour)
	aLazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, aLazyReset.Format(time.RFC3339)))
	aActive := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, aActiveReset.Format(time.RFC3339)))
	bLazy := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`, bReset.Format(time.RFC3339)))
	bActive := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":%q}}}`, bReset.Format(time.RFC3339)))
	host := newProbeFixtureHost()
	host.authFiles = []pluginapi.HostAuthFileEntry{
		{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: 9},
		{ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: 9},
	}
	host.authByIndex = map[string]pluginapi.HostAuthGetResponse{
		"idx-a": {AuthIndex: "idx-a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access-a","refresh_token":"refresh-a","account_id":"acct-a"}`)},
		"idx-b": {AuthIndex: "idx-b", Name: "b.json", JSON: json.RawMessage(`{"access_token":"access-b","refresh_token":"refresh-b","account_id":"acct-b"}`)},
	}
	host.quota = [][]byte{aLazy, bLazy, aActive, bLazy, bActive}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}, "b": {}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{
		{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(9)},
		{ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: intPtr(9)},
	}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	path := filepath.Join(t.TempDir(), "state.json")
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	adapter.bindings = r.bindings
	aBinding, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	r.probeController.SetWindow(aBinding.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(aLazyReset, 80, 5*time.Hour)})
	if err = r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	refresherMu.Lock()
	previousRosterController := globalRosterController
	globalRosterController = nil
	refresherMu.Unlock()
	t.Cleanup(func() { refresherMu.Lock(); globalRosterController = previousRosterController; refresherMu.Unlock() })

	propagationStarted := make(chan struct{})
	releasePropagation := make(chan struct{})
	var propagationCalls atomic.Int32
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error {
		if propagationCalls.Add(1) == 1 {
			close(propagationStarted)
			<-releasePropagation
		}
		return nil
	}
	aDone := make(chan error, 1)
	go func() { aDone <- r.RunProbeDueOnce(context.Background()) }()
	<-propagationStarted

	bBinding, _, err := r.BootstrapBinding(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.RefreshOneAuthID("b"); err != nil {
		t.Fatal(err)
	}
	bPending, ok := r.probeController.Window(bBinding.Instance, ProbeWindowLong)
	if !ok || bPending.State != ProbePendingCheck || !bPending.Deadline.IsZero() {
		t.Fatalf("refresh did not bootstrap B PendingCheck: window=%#v ok=%v", bPending, ok)
	}
	r.wg.Wait() // B's production launch must observe A active before A is released.
	close(releasePropagation)
	if err = <-aDone; err != nil {
		t.Fatal(err)
	}

	counts := probePOSTCountsByAccount(host)
	if counts["acct-a"] != 1 || counts["acct-b"] != 1 {
		t.Fatalf("Probe POST counts = %#v, want one A and one B", counts)
	}
	if window, ok := r.probeController.Window(bBinding.Instance, ProbeWindowLong); !ok || window.State != ProbeConfirmed {
		t.Fatalf("in-memory B window = %#v, ok=%v; want Confirmed", window, ok)
	}
	runtimePersisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := NewStateStore(r.runtimeStore.path, OSFileHooks(), nil).PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if window, ok := persisted.ProbeWindows[bBinding.Instance][ProbeWindowLong]; !ok || window.State != ProbeConfirmed {
		t.Fatalf("persisted B window = %#v, ok=%v; want Confirmed; runtime_windows=%#v file_windows=%#v attempts=%#v", window, ok, runtimePersisted.ProbeWindows, persisted.ProbeWindows, persisted.ProbeAttempts)
	}

	restartAdapter := &rosterCredentialHost{host: host, roster: roster}
	restart, err := NewProductionQuotaRefresher(host, state, restartAdapter, roster, path, func() time.Time { return now.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	restartAdapter.bindings = restart.bindings
	if window, ok := restart.probeController.Window(bBinding.Instance, ProbeWindowLong); !ok || window.State != ProbeConfirmed {
		t.Fatalf("restarted B window = %#v, ok=%v; want Confirmed", window, ok)
	}
	if got := probePOSTCountsByAccount(host); got["acct-a"] != 1 || got["acct-b"] != 1 {
		t.Fatalf("restart changed Probe POST counts: %#v", got)
	}
}

func TestFirstObservedWeeklyLazyWindowConcurrentTriggersSingleFlight(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, binding := newFirstObservedWeeklyLazyRuntime(t, now)
	entered := make(chan struct{})
	release := make(chan struct{})
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-entered
	for i := 0; i < 4; i++ {
		if err := r.RunProbeDueOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	posts, urls := probePOSTCount(host)
	if posts != 1 {
		t.Fatalf("probe POST count during propagation = %d, want 1; urls=%v", posts, urls)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if attempt, ok := persisted.ProbeAttempts[binding.Instance]; ok && nonterminalProbeAttempt(attempt) {
		t.Fatalf("nonterminal attempt survived: %#v", attempt)
	}
}

func TestProbeSnapshotsPreservesMissingUsage(t *testing.T) {
	reset := time.Date(2026, 7, 25, 22, 59, 55, 0, time.UTC)
	snapshot := probeSnapshots(ParsedQuota{LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: reset}})[ProbeWindowLong]
	if snapshot.Usage != nil {
		t.Fatalf("missing usage became %#v", *snapshot.Usage)
	}
}

func TestFirstObservedWeeklyLazyWindowMissingPrecheckUsageDoesNotSend(t *testing.T) {
	now := time.Date(2026, 7, 18, 22, 59, 55, 0, time.UTC)
	r, host, binding := newFirstObservedWeeklyLazyRuntime(t, now)
	reset := now.Add(7 * 24 * time.Hour)
	missing := []byte(fmt.Sprintf(`{"rate_limit":{"secondary_window":{"limit_window_seconds":604800,"reset_at":%q}}}`, reset.Format(time.RFC3339)))
	host.mu.Lock()
	host.quota = [][]byte{missing, missing}
	host.mu.Unlock()

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	posts, urls := probePOSTCount(host)
	if posts != 0 {
		t.Fatalf("missing usage sent %d probe POSTs, want 0; urls=%v", posts, urls)
	}
	window, ok := r.probeController.Window(binding.Instance, ProbeWindowLong)
	if !ok || window.State != ProbeRetryWait {
		t.Fatalf("window = %#v, ok=%v; want RetryWait", window, ok)
	}
}

func TestSuccessfulQuotaRefreshRemovesAbsentFiveHourProbeState(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := newProbeFixtureHost()
	host.quota = [][]byte{
		[]byte(`{"rate_limit":{"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		[]byte(`{}`),
	}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}

	if err := r.RefreshOnce(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour); ok {
		persisted, err := r.runtimeStore.PersistentSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("successful secondary-only refresh retained FiveHour Probe state: persisted=%#v attempts=%#v requests=%#v remaining_quota=%d snapshot=%#v", persisted.ProbeWindows[binding.Instance], persisted.ProbeAttempts[binding.Instance], host.requests, len(host.quota), r.state.Snapshot(now).Accounts)
	}
	snapshot := r.state.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %#v", snapshot.Accounts)
	}
	status, available, reason, _ := accountQueueState(snapshot.Accounts[0], now)
	if status != QueueStatusAvailable || !available || reason != "" {
		t.Fatalf("queue state = %s %v %q", status, available, reason)
	}
}

func TestProbeSequenceCleansMissingFiveHourAfterAttemptCompletes(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := newProbeFixtureHost()
	host.quota = [][]byte{
		[]byte(`{"rate_limit":{"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	r := newDueProbeRuntime(t, now, host)
	binding, ok := r.bindings.Lookup("a")
	if !ok {
		t.Fatal("binding missing")
	}

	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.probeController.Window(binding.Instance, ProbeWindowFiveHour); ok {
		t.Fatal("completed Probe precheck retained absent FiveHour state")
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.ProbeAttempts[binding.Instance]; ok {
		t.Fatal("completed Probe attempt retained")
	}
}

func TestProductionProbeFinalStartDeniedEndToEnd(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{
		auth:  pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)},
		quota: [][]byte{[]byte(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000}}}`)},
	}
	r := newDueProbeRuntime(t, now, host)
	host.mu.Lock()
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	getStarted, releaseGet := host.getStarted, host.releaseGet
	host.mu.Unlock()
	entries := []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterDegraded, BackgroundAllowed: true, Entries: entries})
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-getStarted
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, Entries: entries})
	close(releaseGet)
	if err := <-done; !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("err=%v", err)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("Probe HTTP started after FailClosed publication: %#v", requests)
	}
}

func TestFailClosedHoldsAndRecoveryRecomputesProbe(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 30, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(-time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active}}
	r := newDueProbeRuntime(t, now, host)
	b, _ := r.bindings.Lookup("a")
	if _, err := r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeAttempts[b.Instance] = ProbeAttempt{Instance: b.Instance, AttemptID: "prepared", Phase: ProbeAttemptPrepared, Windows: []ProbeWindowKind{ProbeWindowFiveHour}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	failClosed := ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, BackgroundAllowed: false, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	r.ObserveRosterLifecycle(failClosed)
	if err := r.RunProbeDueOnce(context.Background()); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("FailClosed Probe err=%v", err)
	}
	if len(host.urls) != 0 {
		t.Fatalf("FailClosed started requests=%v", host.urls)
	}
	w, ok := r.probeController.Window(b.Instance, ProbeWindowFiveHour)
	if !ok || w.State != ProbeWaitingRoster || w.Deadline != (time.Time{}) {
		t.Fatalf("held window=%#v ok=%v", w, ok)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok = persisted.ProbeAttempts[b.Instance]; ok {
		t.Fatalf("prepared attempt retained in roster hold: %#v", persisted.ProbeAttempts[b.Instance])
	}

	recovered := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Generation: 1, LifecycleRevision: 3, Entries: failClosed.Entries}
	if err = r.PublishAuthoritativeRoster(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	w, ok = r.probeController.Window(b.Instance, ProbeWindowFiveHour)
	if !ok || w.State != ProbePendingCheck || !w.Deadline.Equal(now) {
		t.Fatalf("recomputed window=%#v ok=%v", w, ok)
	}
	if err = r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := host.urls; len(got) != 3 || got[0] != r.state.Config().QuotaEndpoint || got[1] != codexResetProbeEndpoint || got[2] != r.state.Config().QuotaEndpoint {
		t.Fatalf("recovery requests=%v", got)
	}
}

func TestFailClosedHoldPreservesSentAttemptOnlyForItsWindows(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 40, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	r := newDueProbeRuntime(t, now, host)
	b, _ := r.bindings.Lookup("a")
	r.probeController.SetWindow(b.Instance, ProbeWindowLong, ProbeWindow{State: ProbeRetryWait, Deadline: now.Add(time.Minute), Baseline: UsageOnlyProbeBaseline(55, now.Add(time.Minute))})
	if _, err := r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeWindows = r.probeController.Snapshot()
		s.ProbeAttempts[b.Instance] = ProbeAttempt{Instance: b.Instance, AttemptID: "sent", Phase: ProbeAttemptSent, Windows: []ProbeWindowKind{ProbeWindowFiveHour}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}})
	five, _ := r.probeController.Window(b.Instance, ProbeWindowFiveHour)
	long, _ := r.probeController.Window(b.Instance, ProbeWindowLong)
	if five.State != ProbeSentUnknown || long.State != ProbeWaitingRoster {
		t.Fatalf("five=%s long=%s", five.State, long.State)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ProbeAttempts[b.Instance].AttemptID != "sent" {
		t.Fatalf("sent attempt not retained: %#v", persisted.ProbeAttempts[b.Instance])
	}
}

func TestActiveProbeLifecycleDenialPreservesRosterHold(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 42, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	r := newDueProbeRuntime(t, now, host)
	b, _ := r.bindings.Lookup("a")
	host.mu.Lock()
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	started, releaseGet := host.getStarted, host.releaseGet
	host.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-started
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}})
	close(releaseGet)
	if err := <-done; err == nil {
		t.Fatal("active Probe unexpectedly succeeded after FailClosed")
	}
	time.Sleep(20 * time.Millisecond)
	w, ok := r.probeController.Window(b.Instance, ProbeWindowFiveHour)
	if !ok || w.State != ProbeWaitingRoster || !w.Deadline.IsZero() {
		t.Fatalf("lifecycle-denied Probe escaped roster hold: %#v ok=%v", w, ok)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[b.Instance][ProbeWindowFiveHour]; got.State != ProbeWaitingRoster || !got.Deadline.IsZero() {
		t.Fatalf("persisted lifecycle-denied state=%#v", got)
	}
}

func TestActiveProbeParseFailureAfterFailClosedPreservesRosterHold(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 43, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	r := newDueProbeRuntime(t, now, host)
	b, _ := r.bindings.Lookup("a")
	host.mu.Lock()
	host.auth.JSON = json.RawMessage(`{"access_token":`)
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	started, releaseGet := host.getStarted, host.releaseGet
	host.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-started
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}})
	close(releaseGet)
	if err := <-done; err == nil {
		t.Fatal("parse failure unexpectedly succeeded")
	}
	time.Sleep(20 * time.Millisecond)
	w, ok := r.probeController.Window(b.Instance, ProbeWindowFiveHour)
	if !ok || w.State != ProbeWaitingRoster || !w.Deadline.IsZero() {
		t.Fatalf("parse failure escaped roster hold: %#v ok=%v", w, ok)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[b.Instance][ProbeWindowFiveHour]; got.State != ProbeWaitingRoster || !got.Deadline.IsZero() {
		t.Fatalf("persisted parse failure state=%#v", got)
	}
}

func TestFailClosedRetriesProbeHoldPersistence(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 44, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	r := newDueProbeRuntime(t, now, host)
	b, _ := r.bindings.Lookup("a")
	fail := true
	r.runtimeStore.hooks.Fail = func(op string) error {
		if fail && op == "backup-write" {
			return errors.New("hold persistence failed")
		}
		return nil
	}
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed, Generation: 1, LifecycleRevision: 2, Instances: []string{"a"}})
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ProbeWindows[b.Instance][ProbeWindowFiveHour].State == ProbeWaitingRoster {
		t.Fatal("injected failure unexpectedly persisted roster hold")
	}
	fail = false
	if err = r.RunProbeDueOnce(context.Background()); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("retry path err=%v", err)
	}
	persisted, err = r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[b.Instance][ProbeWindowFiveHour]; got.State != ProbeWaitingRoster || !got.Deadline.IsZero() {
		t.Fatalf("retried hold not durable: %#v", got)
	}
}

func TestRemovedProbeLateGetDoesNotRecreateState(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 45, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "auth.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	entries := []RosterEntry{
		{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(9)},
		{ID: "b", AuthIndex: "b", Provider: "codex", Priority: intPtr(9)},
	}
	roster := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Generation: 1, Entries: entries}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	r, err := NewProductionQuotaRefresher(host, NewPluginState(cfg), adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	if _, _, err = r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	b, _, err := r.BootstrapBinding(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	r.probeController.SetWindow(b.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Deadline: now, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 80, fiveHourSeconds*time.Second)})
	if err = r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.gateAuthIndex = "b"
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	started, releaseGet := host.getStarted, host.releaseGet
	host.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-started
	replacement := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, BackgroundAllowed: true, Health: RosterHealthy, Generation: 2, Entries: entries[:1]}
	if err = r.PublishAuthoritativeRoster(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	close(releaseGet)
	<-done
	time.Sleep(20 * time.Millisecond)
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.ProbeWindows[b.Instance]; ok {
		t.Fatalf("late removed Probe recreated windows=%v", persisted.ProbeWindows[b.Instance])
	}
	if _, ok := persisted.ProbeAttempts[b.Instance]; ok {
		t.Fatalf("late removed Probe recreated attempt=%v", persisted.ProbeAttempts[b.Instance])
	}
	if len(host.urls) != 0 {
		t.Fatalf("removed Probe started OpenAI requests=%v", host.urls)
	}
}

func TestProductionProvisionalProbeMarkerEndToEnd(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(-time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active}}
	r := newDueProbeRuntime(t, now, host)
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = now
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	provisional := r.ProvisionalRoster()
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	if provisional == nil || !r.VerifyProvisionalRoster(context.Background(), *provisional) {
		t.Fatalf("verified provisional=%#v", provisional)
	}
	provisional.BackgroundAllowed = true
	r.ObserveRosterLifecycle(*provisional)
	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests=%#v", requests)
	}
	for i, req := range requests {
		if got := req.Headers.Get(rosterLifecycleRequestHeader); got != rosterLifecycleProvisional {
			t.Fatalf("request %d marker=%q request=%#v", i, got, req)
		}
	}
}

func TestProductionProvisionalVerificationRejectsMismatchWithoutOpenAI(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	legacyPath := filepath.Join(t.TempDir(), "state.json")
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"original","id_token":"` + idToken + `"}`)}}
	state := NewPluginState(DefaultConfig())
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, legacyPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	confirmed := HostRosterSnapshot{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Generation: 3, ConfirmedAt: now, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	if err = r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.auth.JSON = json.RawMessage(`{"access_token":"access","refresh_token":"changed","id_token":"` + idToken + `"}`)
	host.mu.Unlock()
	provisional := r.ProvisionalRoster()
	if provisional == nil {
		t.Fatal("missing provisional snapshot")
	}
	if r.VerifyProvisionalRoster(context.Background(), *provisional) {
		t.Fatal("fingerprint mismatch verified")
	}
	r.ObserveRosterLifecycle(*provisional)
	if err = r.RunProbeDueOnce(context.Background()); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("RunProbeDueOnce err=%v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.requests) != 0 || len(host.urls) != 0 {
		t.Fatalf("mismatch made OpenAI calls requests=%#v urls=%v", host.requests, host.urls)
	}
}

func TestProductionProvisionalVerificationRechecksActualPrecheckFingerprint(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	original := pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"original","id_token":"` + idToken + `"}`)}
	changed := pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"changed-access","refresh_token":"changed","id_token":"` + idToken + `"}`)}
	host := &sequenceProbeHost{auth: original}
	r := newDueProbeRuntime(t, now, host)
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = now
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	provisional := r.ProvisionalRoster()
	if provisional == nil {
		t.Fatal("missing provisional")
	}
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	host.mu.Lock()
	host.authReads = []pluginapi.HostAuthGetResponse{original, changed}
	host.mu.Unlock()
	if !r.VerifyProvisionalRoster(context.Background(), *provisional) {
		t.Fatal("initial verification failed")
	}
	provisional.BackgroundAllowed = true
	r.ObserveRosterLifecycle(*provisional)
	if err := r.RunProbeDueOnce(context.Background()); !errors.Is(err, ErrProvisionalFingerprintMismatch) {
		t.Fatalf("RunProbeDueOnce err=%v", err)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("rotated precheck made OpenAI calls: %#v", requests)
	}
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := r.bindings.Lookup("a")
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; got.State != ProbeWaitingRoster {
		t.Fatalf("mismatch window=%#v", got)
	}
}

func TestProductionProvisionalRequestExpiryDuringPrecheckReturnsWaitingRoster(t *testing.T) {
	confirmedAt := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	initialNow := confirmedAt.Add(provisionalMaxAge - time.Nanosecond)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	auth := pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"r0","id_token":"` + idToken + `"}`)}
	host := &sequenceProbeHost{auth: auth, quota: [][]byte{[]byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, confirmedAt.Add(-time.Hour).Format(time.RFC3339)))}}
	r := newDueProbeRuntime(t, initialNow, host)
	var clock atomic.Int64
	clock.Store(initialNow.UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = confirmedAt
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	provisional := r.ProvisionalRoster()
	if provisional == nil || !r.VerifyConfiguredProvisionalRoster(context.Background(), *provisional) {
		t.Fatalf("verified provisional=%#v", provisional)
	}
	provisional.BackgroundAllowed = true
	r.ObserveRosterLifecycle(*provisional)
	host.mu.Lock()
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	started, release := host.getStarted, host.releaseGet
	host.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-started
	clock.Store(confirmedAt.Add(provisionalMaxAge).UnixNano())
	close(release)
	if err := <-done; !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("RunProbeDueOnce err=%v", err)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("expired provisional made requests=%#v", requests)
	}
	binding, _ := r.bindings.Lookup("a")
	persisted, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ProbeWindows[binding.Instance][ProbeWindowFiveHour]; got.State != ProbeWaitingRoster || !got.Deadline.IsZero() {
		t.Fatalf("expired provisional window=%#v", got)
	}
}

func TestProductionProvisionalVerificationExpiryDuringGetAuthIssuesNoPermit(t *testing.T) {
	confirmedAt := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	initialNow := confirmedAt.Add(provisionalMaxAge - time.Nanosecond)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	auth := pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"r0","id_token":"` + idToken + `"}`)}
	host := &sequenceProbeHost{auth: auth}
	r := newDueProbeRuntime(t, initialNow, host)
	var clock atomic.Int64
	clock.Store(initialNow.UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = confirmedAt
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	provisional := r.ProvisionalRoster()
	if provisional == nil {
		t.Fatal("missing provisional")
	}
	host.mu.Lock()
	host.getStarted = make(chan struct{})
	host.releaseGet = make(chan struct{})
	started, release := host.getStarted, host.releaseGet
	host.mu.Unlock()
	done := make(chan bool, 1)
	go func() { done <- r.VerifyConfiguredProvisionalRoster(context.Background(), *provisional) }()
	<-started
	clock.Store(confirmedAt.Add(provisionalMaxAge).UnixNano())
	close(release)
	if <-done {
		t.Fatal("expired GetAuth verification succeeded")
	}
	r.provisionalMu.Lock()
	permit := r.provisionalPermit
	r.provisionalMu.Unlock()
	if permit {
		t.Fatal("expired verification issued permit")
	}
}

func TestProductionProvisionalConfigDisableLinearizesWithHostStart(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	host := &sequenceProbeHost{doStarted: make(chan struct{}), releaseDo: make(chan struct{})}
	cfg := DefaultConfig()
	cfg.ProbeOnProvisionalRoster = true
	r := NewQuotaRefresher(host, NewPluginState(cfg), func() time.Time { return now })
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityB, Provisional: true, Health: RosterWaiting, BackgroundAllowed: true, ConfirmedAt: now})
	requestDone := make(chan error, 1)
	go func() {
		_, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodPost, URL: codexResetProbeEndpoint}, true)
		requestDone <- err
	}()
	<-host.doStarted
	disabled := make(chan struct{})
	go func() {
		next := r.state.Config()
		next.ProbeOnProvisionalRoster = false
		r.state.ReplaceConfig(next)
		close(disabled)
	}()
	select {
	case <-disabled:
		t.Fatal("config disable crossed in-flight host.Do linearization point")
	case <-time.After(50 * time.Millisecond):
	}
	close(host.releaseDo)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-disabled:
	case <-time.After(time.Second):
		t.Fatal("config disable remained blocked after host.Do")
	}
	if _, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodPost, URL: codexResetProbeEndpoint}, true); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("post-disable request err=%v", err)
	}
}

func TestProductionProvisionalRequestMarkerEndToEnd(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(-time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"r0","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active}}
	r := newDueProbeRuntime(t, now, host)
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = now
	confirmed.Generation = 5
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	provisional := r.ProvisionalRoster()
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	if provisional == nil || !r.VerifyProvisionalRoster(context.Background(), *provisional) {
		t.Fatalf("verified provisional=%#v", provisional)
	}
	provisional.BackgroundAllowed = true
	r.ObserveRosterLifecycle(*provisional)
	if err := r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	requests := append([]pluginapi.HTTPRequest(nil), host.requests...)
	host.mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests=%#v", requests)
	}
	for i, req := range requests {
		if got := req.Headers.Get(rosterLifecycleRequestHeader); got != rosterLifecycleProvisional {
			t.Fatalf("request %d marker=%q", i, got)
		}
	}
}

func TestProductionProvisionalRecoveryRiskStartRunsVerifiedProbe(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(-time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","refresh_token":"r0","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active}}
	r := newDueProbeRuntime(t, now, host)
	confirmed := r.runtimeRoster()
	confirmed.ConfirmedAt = now
	if err := r.PublishAuthoritativeRoster(context.Background(), confirmed); err != nil {
		t.Fatal(err)
	}
	provisional := r.ProvisionalRoster()
	if provisional == nil {
		t.Fatal("missing provisional")
	}
	cfg := r.state.Config()
	cfg.ProbeOnProvisionalRoster = true
	r.state.ReplaceConfig(cfg)
	r.ObserveRosterLifecycle(*provisional)
	controller := NewRosterController(RosterControllerOptions{
		Now:                func() time.Time { return now },
		Provisional:        provisional,
		ProbeOnProvisional: true,
		VerifyProvisional:  r.VerifyConfiguredProvisionalRoster,
		Observe:            r.ObserveRosterLifecycle,
	})
	refresherMu.Lock()
	globalRosterController = controller
	refresherMu.Unlock()
	r.Start()
	t.Cleanup(r.Stop)
	deadline := time.Now().Add(2 * time.Second)
	for {
		host.mu.Lock()
		count := len(host.requests)
		host.mu.Unlock()
		if count == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("verified risk start did not run Probe, requests=%d", count)
		}
		time.Sleep(10 * time.Millisecond)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	for i, req := range host.requests {
		if got := req.Headers.Get(rosterLifecycleRequestHeader); got != rosterLifecycleProvisional {
			t.Fatalf("request %d marker=%q", i, got)
		}
	}
}

func TestProductionProbeRunsWhileNormalRefreshDormant(t *testing.T) {
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

func TestProductionProbeKPointCrashRestartVerifyFirst(t *testing.T) {
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

func TestProbeAttemptIDsMonotonicWithFrozenClock(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(-time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active, lazy, active}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Quota: ParsedQuota{FiveHour: &QuotaWindow{ResetAt: now.Add(-time.Hour), UsedPercent: ptrFloat(80)}}})
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
	if err = r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	first := snap.ProbeAttemptSeq
	w, _ := r.probeController.Window(legacyAuthInstanceID("a"), ProbeWindowFiveHour)
	w.State = ProbePendingCheck
	w.Deadline = now
	r.probeController.SetWindow(legacyAuthInstanceID("a"), ProbeWindowFiveHour, w)
	if err = r.persistProbeWindows(); err != nil {
		t.Fatal(err)
	}
	if err = r.RunProbeDueOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err = r.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.ProbeAttemptSeq <= first {
		t.Fatalf("attempt sequence did not advance: first=%d second=%d", first, snap.ProbeAttemptSeq)
	}
}

func TestProbeDueSingleFlightDuringPropagation(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(-time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{lazy, active}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}}})
	state.UpsertQuota(AccountState{AuthID: "a", AuthIndex: "idx", Quota: ParsedQuota{FiveHour: &QuotaWindow{ResetAt: now.Add(-time.Hour), UsedPercent: ptrFloat(80)}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	if _, _, err = r.BootstrapBinding(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- r.RunProbeDueOnce(context.Background()) }()
	<-entered
	if next := r.probeController.NextDeadline(); !next.IsZero() {
		t.Fatalf("expired deadline visible during propagation: %v", next)
	}
	for i := 0; i < 4; i++ {
		if err := r.RunProbeDueOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	host.mu.Lock()
	calls := len(host.urls)
	host.mu.Unlock()
	if calls != 2 {
		t.Fatalf("timer spin started extra work before propagation completed: calls=%d", calls)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProbeRecoveryExcludesConcurrentDueClaim(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	activeQuota := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "x.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{activeQuota, activeQuota, activeQuota, activeQuota}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}, "b": {}}})
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(9)}, {ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	a, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := r.BootstrapBinding(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	r.probeController.SetWindow(a.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbeRetryWait, Deadline: now})
	r.probeController.SetWindow(b.Instance, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Deadline: now})
	committed, err := r.runtimeStore.Update(func(s *PersistentState) error {
		blocked := s.Bindings["a"]
		blocked.AuthBlocked = true
		s.Bindings["a"] = blocked
		s.ProbeWindows = r.probeController.Snapshot()
		s.ProbeAttempts[a.Instance] = ProbeAttempt{Instance: a.Instance, AttemptID: "old-prepared", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptPrepared, CreatedAt: now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.bindings.mu.Lock()
	r.bindings.bindings = committed.Bindings
	r.bindings.mu.Unlock()

	originalExecute := r.coordinator.opts.Execute
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	r.coordinator.opts.Execute = func(ctx context.Context, intent Intent, held *HeldLease) OperationResult {
		if intent.Class == OperationLegacyRefresh && intent.AttemptID == "block-b" {
			close(blockerStarted)
			<-releaseBlocker
			return OperationResult{Token: intent.Token}
		}
		return originalExecute(ctx, intent, held)
	}
	r.coordinator.activateInstances(map[AuthInstanceID]TierGeneration{a.Instance: TierGeneration(a.Generation), b.Instance: TierGeneration(b.Generation)})
	blocker := r.coordinator.Submit(Intent{Instance: b.Instance, Generation: TierGeneration(b.Generation), Class: OperationLegacyRefresh, Source: LegacyEnvelopeSource, AttemptID: "block-b", Token: b.ExecutionToken(0)})
	<-blockerStarted

	recoverySnapshot := make(chan struct{})
	releaseRecovery := make(chan struct{})
	var firstNow atomic.Bool
	r.now = func() time.Time {
		if firstNow.CompareAndSwap(false, true) {
			close(recoverySnapshot)
			<-releaseRecovery
		}
		return now
	}
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- r.RunProbeRecoveryOnce(context.Background()) }()
	<-recoverySnapshot
	dueDone := make(chan error, 1)
	go func() { dueDone <- r.RunProbeDueOnce(context.Background()) }()
	select {
	case err := <-dueDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		snapshot, snapErr := r.runtimeStore.PersistentSnapshot()
		if snapErr != nil {
			t.Fatal(snapErr)
		}
		if attempt, ok := snapshot.ProbeAttempts[b.Instance]; ok && attempt.Phase == ProbeAttemptPrepared {
			close(releaseRecovery)
			close(releaseBlocker)
			_ = <-recoveryDone
			_ = blocker.Await(context.Background())
			_ = <-dueDone
			t.Fatalf("due claimed %s while recovery owned a stale snapshot", attempt.AttemptID)
		}
		t.Fatal("concurrent due neither returned nor exposed its claim")
	}
	close(releaseRecovery)
	if err = <-recoveryDone; err != nil {
		t.Fatal(err)
	}
	close(releaseBlocker)
	_ = blocker.Await(context.Background())
}

func TestProbeVerifyRejectsReadAtOrBeforeSendFence(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	state := NewPersistentState()
	attempt := ProbeAttempt{Instance: 1, AttemptID: "verify-fence", Phase: ProbeAttemptSentUnknown, SendFenceSeq: 10, VerifyNotBefore: now}
	state.ProbeAttempts[1] = attempt
	if err := store.WriteThrough(state); err != nil {
		t.Fatal(err)
	}
	r := &QuotaRefresher{runtimeStore: store, probeWAL: NewProbeWAL(store), probeFence: NewFenceAllocator(store, state, nil), probeController: NewProbeController(now), now: func() time.Time { return now }, roster: HostRosterSnapshot{Capability: CapabilityA}}
	result := r.runTypedHeld(context.Background(), Intent{Instance: 1, Class: OperationProbeSequence, Source: SourceProbeVerify, StartedAfter: attempt.SendFenceSeq, AttemptID: attempt.AttemptID, Payload: probeSequencePayload{Attempt: attempt, Recovery: true}}, &HeldLease{})
	if result.Err == nil || !strings.Contains(result.Err.Error(), "verify read did not start after send fence") {
		t.Fatalf("verify accepted read_start_seq <= send_fence_seq: result=%#v", result)
	}
	if result.ReadStartSeq != 0 {
		t.Fatalf("forbidden verify published read_start_seq=%d", result.ReadStartSeq)
	}
}

func TestProbeRestartAndDemotionDeleteOrphanState(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "a.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(9)}}}
	path := filepath.Join(t.TempDir(), "state.json")
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	b, _, err := r.BootstrapBinding(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.runtimeStore.Update(func(s *PersistentState) error {
		s.ProbeWindows[999] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: {State: ProbeRetryWait, Deadline: now}}
		s.ProbeAttempts[999] = ProbeAttempt{Instance: 999, AttemptID: "orphan", Phase: ProbeAttemptSent, SendFenceSeq: 2, VerifyNotBefore: now}
		s.ProbeWindows[b.Instance] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: {State: ProbeRetryWait, Deadline: now.Add(time.Hour)}}
		s.ProbeAttempts[b.Instance] = ProbeAttempt{Instance: b.Instance, AttemptID: "demoted", Phase: ProbeAttemptSent, SendFenceSeq: 3, VerifyNotBefore: now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter2 := &rosterCredentialHost{host: host, roster: roster}
	restart, err := NewProductionQuotaRefresher(host, state, adapter2, roster, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter2.bindings = restart.bindings
	restart.rosterMu.Lock()
	restart.roster = HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "b", AuthIndex: "bidx", Provider: "codex", Priority: intPtr(10)}}}
	restart.rosterMu.Unlock()
	if err = restart.RunProbeRecoveryOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := restart.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.ProbeWindows) != 0 || len(snap.ProbeAttempts) != 0 {
		t.Fatalf("orphan state retained after restart/demotion: windows=%v attempts=%v", snap.ProbeWindows, snap.ProbeAttempts)
	}
	host.mu.Lock()
	calls := len(host.urls)
	host.mu.Unlock()
	if calls != 0 {
		t.Fatalf("orphan cleanup issued requests: %v", host.urls)
	}
}

func TestProbeDueFailureContinuesOtherConfirmedInstance(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	lazy := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(-time.Hour).Format(time.RFC3339)))
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "x.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{[]byte(`bad`), lazy, active}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"a": {}, "b": {}}})
	for _, id := range []string{"a", "b"} {
		state.UpsertQuota(AccountState{AuthID: id, AuthIndex: "idx-" + id, Quota: ParsedQuota{FiveHour: &QuotaWindow{ResetAt: now.Add(-time.Hour), UsedPercent: ptrFloat(80)}}})
	}
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(9)}, {ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: intPtr(9)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, filepath.Join(t.TempDir(), "state.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	for _, id := range []string{"a", "b"} {
		if _, _, err = r.BootstrapBinding(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	r.coordinator.opts.PropagationWait = func(context.Context, time.Duration) error { return nil }
	if err = r.RunProbeDueOnce(context.Background()); err == nil {
		t.Fatal("first instance failure was not surfaced")
	}
	host.mu.Lock()
	posts := 0
	for _, u := range host.urls {
		if u == codexResetProbeEndpoint {
			posts++
		}
	}
	host.mu.Unlock()
	if posts != 1 {
		t.Fatalf("other confirmed instance was starved: urls=%v", host.urls)
	}
}

func TestProbeRecoveryFailureContinuesOtherConfirmedInstance(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct"})
	active := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":%q}}}`, now.Add(4*time.Hour).Format(time.RFC3339)))
	host := &sequenceProbeHost{auth: pluginapi.HostAuthGetResponse{AuthIndex: "idx", Name: "x.json", JSON: json.RawMessage(`{"access_token":"access","id_token":"` + idToken + `"}`)}, quota: [][]byte{[]byte(`bad`), active}}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	state := NewPluginState(cfg)
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(9)}, {ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: intPtr(9)}}}
	path := filepath.Join(t.TempDir(), "state.json")
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, state, adapter, roster, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	var bs []RuntimeBinding
	for _, id := range []string{"a", "b"} {
		b, _, e := r.BootstrapBinding(context.Background(), id)
		if e != nil {
			t.Fatal(e)
		}
		bs = append(bs, b)
	}
	_, err = r.runtimeStore.Update(func(s *PersistentState) error {
		s.ReservedCeiling = 100
		for n, b := range bs {
			s.ProbeWindows[b.Instance] = map[ProbeWindowKind]ProbeWindow{ProbeWindowFiveHour: {State: ProbeSentUnknown, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 80, 5*time.Hour)}}
			s.ProbeAttempts[b.Instance] = ProbeAttempt{Instance: b.Instance, AttemptID: fmt.Sprintf("r%d", n), Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: uint64(n + 1), CreatedAt: now.Add(-time.Hour), VerifyNotBefore: now}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter2 := &rosterCredentialHost{host: host, roster: roster}
	restart, err := NewProductionQuotaRefresher(host, state, adapter2, roster, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adapter2.bindings = restart.bindings
	if err = restart.RunProbeRecoveryOnce(context.Background()); err == nil {
		t.Fatal("recovery failure was not surfaced")
	}
	snap, err := restart.runtimeStore.PersistentSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.ProbeAttempts) != 1 {
		t.Fatalf("other recovery was starved: attempts=%v urls=%v windows=%v", snap.ProbeAttempts, host.urls, snap.ProbeWindows)
	}
}

func TestProductionProbeHasNoSplitSendExecutor(t *testing.T) {
	raw, err := os.ReadFile("probe_runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	if strings.Contains(source, "case OperationProbeSend:") || strings.Contains(source, "type probeSendPayload struct") {
		t.Fatal("superseded split probe-send production executor remains")
	}
}
