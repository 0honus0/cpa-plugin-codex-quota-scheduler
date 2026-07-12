package main

import (
	"testing"
	"time"
)

func refreshTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	got, err := time.Parse("2006-01-02 15:04:05.999999999", value)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func requireRefreshIntent(t *testing.T, intents []Intent, source IntentSource) {
	t.Helper()
	if len(intents) != 1 || IntentSource(intents[0].Source) != source {
		t.Fatalf("intents = %#v, want one %q intent", intents, source)
	}
}

func TestSuiteRefresh(t *testing.T) {
	t.Run("exact virtual timeline", func(t *testing.T) {
		start := refreshTestTime(t, "2026-07-12 10:00:00.000000000")
		controller := NewRefreshController(30*time.Minute, time.Hour)

		requireRefreshIntent(t, controller.OnPick(start, CacheSnapshot{Initial: true}), IntentSourceInitial)
		if got := controller.Mode(start); got != RefreshModeActive {
			t.Fatalf("mode at 10:00 = %q, want Active", got)
		}
		if got := controller.OnPick(start.Add(20*time.Minute), CacheSnapshot{}); len(got) != 0 {
			t.Fatalf("10:20 activity-only intents = %#v", got)
		}
		requireRefreshIntent(t, controller.OnDeadline(start.Add(30*time.Minute)), IntentSourceInterval)
		requireRefreshIntent(t, controller.OnDeadline(start.Add(time.Hour)), IntentSourceInterval)
		if got := controller.OnDeadline(start.Add(80 * time.Minute)); len(got) != 0 {
			t.Fatalf("11:20 dormant intents = %#v", got)
		}
		if got := controller.Mode(start.Add(80 * time.Minute)); got != RefreshModeDormant {
			t.Fatalf("mode at 11:20 = %q, want Dormant", got)
		}
	})

	t.Run("interval and active-window boundaries", func(t *testing.T) {
		start := refreshTestTime(t, "2026-07-12 10:00:00.000000000")
		controller := NewRefreshController(30*time.Minute, time.Hour)
		controller.OnPick(start, CacheSnapshot{Initial: true})
		before := time.Nanosecond

		if got := controller.OnDeadline(start.Add(30*time.Minute - before)); len(got) != 0 {
			t.Fatalf("before interval intents = %#v", got)
		}
		requireRefreshIntent(t, controller.OnDeadline(start.Add(30*time.Minute)), IntentSourceInterval)
		if got := controller.OnDeadline(start.Add(30*time.Minute + before)); len(got) != 0 {
			t.Fatalf("after consumed interval intents = %#v", got)
		}

		if got := controller.Mode(start.Add(time.Hour - before)); got != RefreshModeActive {
			t.Fatalf("before active deadline mode = %q", got)
		}
		if got := controller.Mode(start.Add(time.Hour)); got != RefreshModeDormant {
			t.Fatalf("at active deadline mode = %q", got)
		}
		if got := controller.OnDeadline(start.Add(time.Hour + before)); len(got) != 0 {
			t.Fatalf("after active deadline intents = %#v", got)
		}
	})

	t.Run("stale-after and aging never create deadlines", func(t *testing.T) {
		start := refreshTestTime(t, "2026-07-12 10:00:00.000000000")
		controller := NewRefreshController(30*time.Minute, time.Hour)
		controller.OnPick(start, CacheSnapshot{Initial: true, StaleAfter: 5 * time.Minute})
		if got := controller.NextDeadline(start); !got.Equal(start.Add(30 * time.Minute)) {
			t.Fatalf("next deadline = %s, want interval deadline", got)
		}
		if got := controller.OnDeadline(start.Add(5 * time.Minute)); len(got) != 0 {
			t.Fatalf("stale-only deadline intents = %#v", got)
		}
	})

	t.Run("dormant retains cache and emits no normal request", func(t *testing.T) {
		start := refreshTestTime(t, "2026-07-12 10:00:00.000000000")
		cfg := DefaultConfig()
		store := NewPluginState(cfg)
		store.UpsertQuota(AccountState{AuthID: "retained", Provider: "codex", LastSuccessAt: start})
		store.RecordCodexActivity(start)
		host := &fakeHostClient{}
		refresher := NewQuotaRefresher(host, store, func() time.Time { return start.Add(cfg.RefreshActiveWindow) })
		t.Cleanup(refresher.Stop)

		if err := refresher.RefreshDueOnce(); err != nil {
			t.Fatal(err)
		}
		if host.listCallCount() != 0 || host.httpCallCount() != 0 {
			t.Fatalf("dormant normal refresh made list/http calls = %d/%d", host.listCallCount(), host.httpCallCount())
		}
		if got := store.Snapshot(start.Add(cfg.RefreshActiveWindow)).Accounts; len(got) != 1 || got[0].AuthID != "retained" {
			t.Fatalf("dormant cache/accounts = %#v", got)
		}
	})
}

func TestRefreshSourcePriorityTruthTable(t *testing.T) {
	tests := []struct {
		initial, staleRecovery, interval bool
		want                             IntentSource
	}{
		{false, false, false, IntentSourceNone},
		{false, false, true, IntentSourceInterval},
		{false, true, false, IntentSourceStaleRecovery},
		{false, true, true, IntentSourceStaleRecovery},
		{true, false, false, IntentSourceInitial},
		{true, false, true, IntentSourceInitial},
		{true, true, false, IntentSourceInitial},
		{true, true, true, IntentSourceInitial},
	}
	for _, tc := range tests {
		if got := UniqueSchedulerSource(tc.initial, tc.staleRecovery, tc.interval); got != tc.want {
			t.Errorf("UniqueSchedulerSource(%t,%t,%t) = %q, want %q", tc.initial, tc.staleRecovery, tc.interval, got, tc.want)
		}
	}
}

func TestNormalRefreshDeadlineHasSingleControllerOwner(t *testing.T) {
	start := refreshTestTime(t, "2026-07-12 10:00:00.000000000")
	cfg := DefaultConfig()
	cfg.QuotaRefreshInterval = 30 * time.Minute
	cfg.StaleAfter = 31 * time.Minute
	store := NewPluginState(cfg)
	store.RecordCodexActivity(start)
	store.UpsertQuota(AccountState{AuthID: "a", Provider: "codex", LastSuccessAt: start})

	if got := store.NextRefreshDueAt(start); !got.IsZero() {
		t.Fatalf("legacy normal-refresh deadline = %s, want zero; controller must be sole owner", got)
	}
	controller := NewRefreshController(cfg.QuotaRefreshInterval, cfg.RefreshActiveWindow)
	controller.OnPick(start, CacheSnapshot{})
	if got := controller.NextDeadline(start); !got.Equal(start.Add(30 * time.Minute)) {
		t.Fatalf("controller deadline = %s", got)
	}
}

func TestRefreshActiveWindowDeadlineIsExclusive(t *testing.T) {
	start := refreshTestTime(t, "2026-07-12 10:00:00.000000000")
	store := NewPluginState(DefaultConfig())
	store.RecordCodexActivity(start)
	if store.RefreshActive(start.Add(time.Hour)) {
		t.Fatal("refresh active at exact active-window deadline")
	}
}

func TestMockGroupERefresh(t *testing.T) { TestSuiteRefresh(t) }
