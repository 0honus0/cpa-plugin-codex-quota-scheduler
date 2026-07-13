package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeffery/codex-quota-scheduler/testsupport"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type sequenceProbeHost struct {
	mu         sync.Mutex
	auth       pluginapi.HostAuthGetResponse
	authReads  []pluginapi.HostAuthGetResponse
	quota      [][]byte
	urls       []string
	requests   []pluginapi.HTTPRequest
	gets       int
	getStarted chan struct{}
	releaseGet chan struct{}
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
	h.mu.Lock()
	h.gets++
	auth, started, release := h.auth, h.getStarted, h.releaseGet
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
	defer h.mu.Unlock()
	h.urls = append(h.urls, req.URL)
	h.requests = append(h.requests, req)
	if req.URL == codexResetProbeEndpoint {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"usage":{"total_tokens":1}}`)}, nil
	}
	body := h.quota[0]
	h.quota = h.quota[1:]
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body}, nil
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

func TestFailClosedHoldsAndRecoveryRecomputesProbe(t *testing.T) { //inv:INV-21,INV-35
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

func TestFailClosedHoldPreservesSentAttemptOnlyForItsWindows(t *testing.T) { //inv:INV-17,INV-35
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

func TestActiveProbeLifecycleDenialPreservesRosterHold(t *testing.T) { //inv:INV-03,INV-35
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

func TestActiveProbeParseFailureAfterFailClosedPreservesRosterHold(t *testing.T) { //inv:INV-03,INV-35
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

func TestFailClosedRetriesProbeHoldPersistence(t *testing.T) { //inv:INV-19,INV-35
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

func TestRemovedProbeLateGetDoesNotRecreateState(t *testing.T) { //inv:INV-03,INV-20
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

func TestProductionProvisionalVerificationRejectsMismatchWithoutOpenAI(t *testing.T) { //inv:INV-02,INV-34,INV-35
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

func TestProductionProvisionalVerificationRechecksActualPrecheckFingerprint(t *testing.T) { //inv:INV-02,INV-34,INV-35
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

func TestProductionProvisionalRequestMarkerEndToEnd(t *testing.T) { //inv:INV-02,INV-34
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

func TestProductionProvisionalRecoveryRiskStartRunsVerifiedProbe(t *testing.T) { //inv:INV-02,INV-34,INV-35
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

func TestProductionDueConsumesSchedulerDeadlineDuringPropagation(t *testing.T) {
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
