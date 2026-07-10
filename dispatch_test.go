package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func withGlobalRefresherForTest(t *testing.T, store *PluginState, refresher *QuotaRefresher) {
	t.Helper()

	refresherMu.Lock()
	previousState := globalState
	previousRefresher := globalRefresher
	previousDefaultStatePath := defaultStatePath
	globalState = store
	globalRefresher = refresher
	defaultStatePath = func() string { return t.TempDir() + "\\state.json" }
	refresherMu.Unlock()

	t.Cleanup(func() {
		if refresher != nil {
			refresher.Stop()
		}
		refresherMu.Lock()
		globalState = previousState
		globalRefresher = previousRefresher
		defaultStatePath = previousDefaultStatePath
		refresherMu.Unlock()
	})
}

func lifecyclePayload(t *testing.T, configYAML string) []byte {
	t.Helper()
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte(configYAML)})
	if err != nil {
		t.Fatalf("marshal lifecycle payload: %v", err)
	}
	return raw
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return condition()
}

func TestPluginRegisterStartsRefresherWithoutStartupRefreshByDefault(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	host := &fakeHostClient{}
	refresher := NewQuotaRefresher(host, store, time.Now)
	withGlobalRefresherForTest(t, store, refresher)

	if _, err := handleMethod(pluginabi.MethodPluginRegister, lifecyclePayload(t, "quota_refresh_interval: 1h\n")); err != nil {
		t.Fatalf("plugin.register returned error: %v", err)
	}

	refresher.mu.Lock()
	running := refresher.running
	refresher.mu.Unlock()
	if !running {
		t.Fatal("refresher running = false, want true")
	}
	if refreshed := waitForCondition(t, 50*time.Millisecond, func() bool { return host.listCallCount() > 0 }); refreshed {
		t.Fatalf("ListAuths calls = %d, want 0 startup refreshes", host.listCallCount())
	}
}

func TestPluginRegisterRefreshesOnStartupWhenConfigured(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	highToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-high"})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "high", AuthIndex: "idx-high", Provider: "codex"},
			{ID: "low", AuthIndex: "idx-low", Provider: "codex"},
		},
		authJSON:  map[string]json.RawMessage{"idx-high": json.RawMessage(`{"access_token":"access-high","id_token":"` + highToken + `"}`)},
		httpBody:  []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		doStarted: make(chan struct{}, 1),
		releaseDo: make(chan struct{}),
	}
	refresher := NewQuotaRefresher(host, store, time.Now)
	withGlobalRefresherForTest(t, store, refresher)

	if _, err := handleMethod(pluginabi.MethodPluginRegister, lifecyclePayload(t, "quota_refresh_interval: 1h\nrefresh_on_startup: true\n")); err != nil {
		t.Fatalf("plugin.register returned error: %v", err)
	}

	refresher.wg.Wait()
	if host.listCallCount() != 0 {
		t.Fatalf("ListAuths calls = %d, want no scan before scheduler admission", host.listCallCount())
	}
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Provider: "codex", Priority: 10},
		{ID: "low", Provider: "codex", Priority: 1},
	}})
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatal(err)
	}
	<-host.doStarted
	close(host.releaseDo)
	refresher.wg.Wait()
	snapshot := store.Snapshot(time.Now())
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].AuthID != "high" {
		t.Fatalf("accounts = %#v, want only admitted high account", snapshot.Accounts)
	}
}

func TestSchedulerPickReplacesAdmissionBeforeRefresh(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	refresher := NewQuotaRefresher(&fakeHostClient{}, store, time.Now)
	withGlobalRefresherForTest(t, store, refresher)
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Provider: "codex", Priority: 1},
		{ID: "low", Provider: "codex", Priority: 0},
	}})
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatal(err)
	}
	admission := store.CPAAdmission()
	if admission.Priority != 1 || len(admission.AuthIDs) != 1 {
		t.Fatalf("admission = %#v", admission)
	}
}

func TestSchedulerAdmissionLogDeduplicatesCandidateCounts(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	withGlobalRefresherForTest(t, store, nil)
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high", Provider: "codex", Priority: 10},
		{ID: "high", Provider: "codex", Priority: 10},
		{ID: "low", Provider: "codex", Priority: 1},
		{ID: "other", Provider: "openai", Priority: 99},
	}})
	if _, err := handleSchedulerPick(raw); err != nil {
		t.Fatal(err)
	}
	for _, entry := range store.Snapshot(time.Now()).Logs {
		if entry.Event != "scheduler.cpa_admission_updated" {
			continue
		}
		if entry.Fields["admitted_count"] != 1 || entry.Fields["excluded_count"] != 1 {
			t.Fatalf("admission log fields = %#v, want deduplicated counts 1/1", entry.Fields)
		}
		return
	}
	t.Fatal("scheduler.cpa_admission_updated log not found")
}

func TestConcurrentSchedulerPicksCoalesceLatestAdmission(t *testing.T) {
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	aToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-a"})
	bToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-b"})
	store := NewPluginState(DefaultConfig())
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "a", AuthIndex: "idx-a", Provider: "codex"},
			{ID: "b", AuthIndex: "idx-b", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-a": json.RawMessage(`{"access_token":"access-a","id_token":"` + aToken + `"}`),
			"idx-b": json.RawMessage(`{"access_token":"access-b","id_token":"` + bToken + `"}`),
		},
		httpBody:  []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
		doStarted: make(chan struct{}, 2),
		releaseDo: make(chan struct{}),
	}
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	withGlobalRefresherForTest(t, store, refresher)
	released := false
	t.Cleanup(func() {
		if !released {
			close(host.releaseDo)
		}
	})

	request := func(authID string, priority int) {
		raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: authID, Provider: "codex", Priority: priority}}})
		if _, err := handleSchedulerPick(raw); err != nil {
			t.Fatal(err)
		}
	}
	request("a", 1)
	<-host.doStarted
	requestBDone := make(chan error, 1)
	go func() {
		raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "b", Provider: "codex", Priority: 2}}})
		_, err := handleSchedulerPick(raw)
		requestBDone <- err
	}()
	if !waitForCondition(t, time.Second, func() bool { return store.cpaAdmissionIntent.Load() > 0 }) {
		t.Fatal("latest scheduler admission did not become pending")
	}
	close(host.releaseDo)
	released = true
	if err := <-requestBDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.doStarted:
	case <-time.After(time.Second):
		t.Fatal("latest scheduler admission was not refreshed after in-flight refresh completed")
	}
	refresher.wg.Wait()
	admission := store.CPAAdmission()
	if admission.Priority != 2 || !store.IsAuthAdmitted("b") || store.IsAuthAdmitted("a") {
		t.Fatalf("admission = %#v, want latest b priority 2", admission)
	}
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].AuthID != "b" {
		t.Fatalf("accounts = %#v, want only latest admitted b", snapshot.Accounts)
	}
}

func TestSchedulerPickRecordsCodexActivityAndRequestsDueRefresh(t *testing.T) {
	now := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{
		AuthID:        "auth-1",
		AuthIndex:     "idx-1",
		Provider:      "codex",
		LastSuccessAt: now.Add(-6 * time.Hour),
	})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{
			ID: "auth-1", AuthIndex: "idx-1", Provider: "codex",
		}},
	}
	refresher := NewQuotaRefresher(host, store, func() time.Time { return now })
	withGlobalRefresherForTest(t, store, refresher)

	req, err := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{{
			ID: "auth-1", Provider: "codex",
		}},
	})
	if err != nil {
		t.Fatalf("marshal pick request: %v", err)
	}
	if _, err := handleSchedulerPick(req); err != nil {
		t.Fatalf("scheduler.pick returned error: %v", err)
	}

	snapshot := store.Snapshot(now)
	if snapshot.LastCodexActivityAt.IsZero() {
		t.Fatal("LastCodexActivityAt is zero, want Codex activity recorded")
	}
	if !store.RefreshActive(time.Now()) {
		t.Fatalf("RefreshActive = false after scheduler.pick; LastCodexActivityAt=%s", snapshot.LastCodexActivityAt)
	}
	if refreshed := waitForCondition(t, time.Second, func() bool { return host.listCallCount() > 0 }); !refreshed {
		t.Fatal("ListAuths was not called, want due refresh request")
	}
}

func TestSchedulerPickRefreshesOnlyActivePriorityCandidates(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	highToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-high"})
	lowToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-low"})
	store := NewPluginState(DefaultConfig())
	highBefore := now.Add(-6 * time.Hour)
	lowBefore := now.Add(-6 * time.Hour)
	store.UpsertQuota(AccountState{AuthID: "high", AuthIndex: "idx-high", Provider: "codex", LastSuccessAt: highBefore, Priority: 10})
	store.UpsertQuota(AccountState{AuthID: "low", AuthIndex: "idx-low", Provider: "codex", LastSuccessAt: lowBefore, Priority: 1})
	host := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{
			{ID: "high", AuthIndex: "idx-high", Provider: "codex"},
			{ID: "low", AuthIndex: "idx-low", Provider: "codex"},
		},
		authJSON: map[string]json.RawMessage{
			"idx-high": json.RawMessage(`{"access_token":"access-high","id_token":"` + highToken + `"}`),
			"idx-low":  json.RawMessage(`{"access_token":"access-low","id_token":"` + lowToken + `"}`),
		},
		httpBody: []byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`),
	}
	refresher := NewQuotaRefresher(host, store, time.Now)
	withGlobalRefresherForTest(t, store, refresher)

	req, err := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "high", Provider: "codex", Priority: 10},
			{ID: "low", Provider: "codex", Priority: 1},
		},
	})
	if err != nil {
		t.Fatalf("marshal pick request: %v", err)
	}
	if _, err := handleSchedulerPick(req); err != nil {
		t.Fatalf("scheduler.pick returned error: %v", err)
	}

	if refreshed := waitForCondition(t, time.Second, func() bool {
		return accountByAuthID(t, store.Snapshot(time.Now()), "high").LastSuccessAt.After(highBefore)
	}); !refreshed {
		t.Fatal("high priority account was not refreshed")
	}
	for _, account := range store.Snapshot(time.Now()).Accounts {
		if account.AuthID == "low" {
			t.Fatalf("low-priority account remained in state: %#v", account)
		}
	}
}

func TestLogSchedulerDecisionIncludesDetailedFallbackReason(t *testing.T) {
	now := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	decision := PickDecision{
		Handled:         true,
		DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst,
		Reason:          "fallback_fill_first",
		Ordered: []ScheduledAccount{
			{AuthID: "five-hour", QueueStatus: QueueStatusFiveHourExhausted, UnavailableReason: "five_hour_exhausted"},
			{AuthID: "weekly", QueueStatus: QueueStatusLongWindowExhausted, UnavailableReason: "weekly_exhausted"},
		},
	}

	logSchedulerDecision(store, pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-5-codex"}, decision, now)

	logs := store.Snapshot(now).Logs
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	fields := logs[0].Fields
	if fields["reason"] != "fallback_fill_first" || fields["fallback"] != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("fields = %#v, want fallback reason and builtin", fields)
	}
	if fields["ordered_count"] != 2 {
		t.Fatalf("ordered_count = %#v, want 2; fields=%#v", fields["ordered_count"], fields)
	}
	if fields["unavailable_summary"] == "" {
		t.Fatalf("unavailable_summary empty; fields=%#v", fields)
	}
}

func TestLogSchedulerDecisionIncludesSelectedAccountContext(t *testing.T) {
	now := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	decision := PickDecision{
		AuthID:  "auth-1",
		Handled: true,
		Reason:  "selected",
		Ordered: []ScheduledAccount{
			{AuthID: "auth-1", QueueStatus: QueueStatusAvailable, Available: true},
		},
	}

	logSchedulerDecision(store, pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-5-codex"}, decision, now)

	fields := store.Snapshot(now).Logs[0].Fields
	if fields["auth_id"] != "auth-1" || fields["selected_queue_status"] != string(QueueStatusAvailable) || fields["ordered_count"] != 1 {
		t.Fatalf("fields = %#v, want selected account context", fields)
	}
}
