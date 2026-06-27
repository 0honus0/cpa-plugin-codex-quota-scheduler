package main

import (
	"testing"
	"time"
)

func TestPluginStateUpsertsAndSnapshotsAccounts(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{
		AuthID:    "auth-1",
		AuthIndex: "idx-1",
		Provider:  "codex",
		Priority:  5,
		Family:    AccountFamilyWeekly,
		Quota: ParsedQuota{
			Family:     AccountFamilyWeekly,
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, ResetAt: now.Add(time.Hour)},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: now.Add(24 * time.Hour)},
		},
		LastRefreshAt: now,
		LastSuccessAt: now,
	})
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	if snapshot.Accounts[0].AuthID != "auth-1" {
		t.Fatalf("account = %#v", snapshot.Accounts[0])
	}
}

func TestPluginStateNormalizesConfigAtBoundaries(t *testing.T) {
	store := NewPluginState(Config{})
	if store.Config().MaxLogEntries != DefaultConfig().MaxLogEntries || store.Config().LogRetention != DefaultConfig().LogRetention {
		t.Fatalf("initial config = %#v, want normalized defaults", store.Config())
	}

	store.ReplaceConfig(Config{})
	if store.Config().QuotaRefreshInterval != DefaultConfig().QuotaRefreshInterval || store.Config().StaleAfter != DefaultConfig().StaleAfter {
		t.Fatalf("replaced config = %#v, want normalized defaults", store.Config())
	}
}

func TestPluginStateMarksStale(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StaleAfter = time.Minute
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{AuthID: "auth-1", LastSuccessAt: now.Add(-2 * time.Minute)})
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 || !snapshot.Accounts[0].Stale {
		t.Fatalf("snapshot = %#v", snapshot.Accounts)
	}
}

func TestPluginStateTemporaryExhaustedIgnoresEmptyAuthID(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.MarkAccountTemporaryExhausted("", now.Add(time.Hour), "usage exhausted")
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 0 {
		t.Fatalf("accounts = %#v, want none", snapshot.Accounts)
	}
}

func TestPluginStateMarksTemporaryExhaustedByAuthIndex(t *testing.T) {
	store := NewPluginState(DefaultConfig())
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1"})
	store.MarkAccountTemporaryExhaustedByAuthIndex("idx-1", resetAt, "usage exhausted")
	snapshot := store.Snapshot(now)
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(snapshot.Accounts))
	}
	account := snapshot.Accounts[0]
	if !account.TemporaryExhausted || !account.TemporaryResetAt.Equal(resetAt) || account.LastError != "usage exhausted" {
		t.Fatalf("account = %#v", account)
	}
}

func TestPluginStateOpenCircuitIgnoresEarlySuccess(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CircuitFailureThreshold = 1
	cfg.CircuitOpenDuration = 10 * time.Minute
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})

	failed, ok := store.RecordAccountFailure("auth-1", "", "usage_limit_reached", time.Time{}, now)
	if !ok || failed.Circuit.State != CircuitStateOpen {
		t.Fatalf("failed account = %#v, ok=%t; want open circuit", failed, ok)
	}
	success, ok := store.RecordAccountSuccess("auth-1", "", now.Add(time.Minute))
	if !ok {
		t.Fatalf("RecordAccountSuccess returned ok=false")
	}
	if success.Circuit.State != CircuitStateOpen || success.Circuit.EffectiveState != CircuitStateOpen {
		t.Fatalf("circuit = %#v, want still open before next probe", success.Circuit)
	}
	if success.LastError == "" {
		t.Fatalf("LastError was cleared while circuit is still open: %#v", success)
	}
}

func TestPluginStateHalfOpenSuccessClosesCircuit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CircuitFailureThreshold = 1
	cfg.CircuitOpenDuration = 10 * time.Minute
	cfg.CircuitHalfOpenSuccessThreshold = 1
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	store.RecordAccountFailure("auth-1", "", "usage_limit_reached", time.Time{}, now)

	success, ok := store.RecordAccountSuccess("auth-1", "", now.Add(11*time.Minute))
	if !ok {
		t.Fatalf("RecordAccountSuccess returned ok=false")
	}
	if success.Circuit.State != CircuitStateClosed || success.LastError != "" {
		t.Fatalf("account = %#v, want half-open probe success to close circuit", success)
	}
}

func TestPluginStateLogRetentionKeepsNewestEntriesByCount(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLogEntries = 3
	cfg.LogRetention = 24 * time.Hour
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		store.RecordLog("info", "event", string(rune('a'+i)), nil, now.Add(time.Duration(i)*time.Minute))
	}

	logs := store.Snapshot(now.Add(5 * time.Minute)).Logs
	if len(logs) != 3 {
		t.Fatalf("logs len = %d, want 3: %#v", len(logs), logs)
	}
	if logs[0].Message != "c" || logs[2].Message != "e" {
		t.Fatalf("logs = %#v, want newest c,d,e", logs)
	}
}

func TestPluginStateLogRetentionDropsEntriesOlderThanRetention(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLogEntries = 10
	cfg.LogRetention = time.Hour
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)

	store.RecordLog("info", "event", "old", nil, now.Add(-2*time.Hour))
	store.RecordLog("info", "event", "new", nil, now.Add(-30*time.Minute))

	logs := store.Snapshot(now).Logs
	if len(logs) != 1 || logs[0].Message != "new" {
		t.Fatalf("logs = %#v, want only new log", logs)
	}
}

func TestPluginStateLogRetentionDropsOldEntriesEvenWhenOutOfOrder(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxLogEntries = 10
	cfg.LogRetention = time.Hour
	store := NewPluginState(cfg)
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)

	store.RecordLog("info", "event", "new-before", nil, now.Add(-30*time.Minute))
	store.RecordLog("info", "event", "old-after", nil, now.Add(-2*time.Hour))
	store.RecordLog("info", "event", "new-after", nil, now.Add(-10*time.Minute))

	logs := store.Snapshot(now).Logs
	if len(logs) != 2 {
		t.Fatalf("logs len = %d, want 2: %#v", len(logs), logs)
	}
	if logs[0].Message != "new-before" || logs[1].Message != "new-after" {
		t.Fatalf("logs = %#v, want only non-expired entries in original order", logs)
	}
}

func TestRecordCodexActivityControlsActiveWindow(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	if store.RefreshActive(now) {
		t.Fatal("RefreshActive before activity = true, want false")
	}
	store.RecordCodexActivity(now)
	if !store.RefreshActive(now.Add(59 * time.Minute)) {
		t.Fatal("RefreshActive inside 1h window = false, want true")
	}
	if store.RefreshActive(now.Add(time.Hour + time.Second)) {
		t.Fatal("RefreshActive after 1h window = true, want false")
	}
}

func TestRecordRefreshFailureSchedulesRetry(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	updated, ok := store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureTransient, "request failed", now)
	if !ok {
		t.Fatal("RecordRefreshFailure returned ok=false")
	}
	if updated.Refresh.RetryAttempt != 1 {
		t.Fatalf("RetryAttempt = %d, want 1", updated.Refresh.RetryAttempt)
	}
	if !updated.Refresh.NextRetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("NextRetryAt = %s, want %s", updated.Refresh.NextRetryAt, now.Add(time.Minute))
	}
	if updated.Refresh.AuthFailure {
		t.Fatal("AuthFailure = true, want false")
	}
}

func TestFutureRetryPreventsNeverRefreshedDue(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureTransient, "request failed", now)

	due := store.DueAccounts(now.Add(30 * time.Second))
	if len(due) != 0 {
		t.Fatalf("due accounts before retry time = %#v, want none", due)
	}

	due = store.DueAccounts(now.Add(time.Minute))
	if len(due) != 1 || due[0].Refresh.DueReason != "retry_due" {
		t.Fatalf("due accounts at retry time = %#v, want retry_due account", due)
	}
}

func TestCircuitProbeControlsRefreshDue(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	account := AccountState{
		AuthID:   "auth-1",
		Provider: "codex",
		Circuit: CircuitBreakerState{
			State:       CircuitStateOpen,
			NextProbeAt: now.Add(2 * time.Minute),
		},
	}
	if due, reason := accountRefreshDue(account, cfg, now); due || reason != "circuit_wait" {
		t.Fatalf("accountRefreshDue before probe = %t, %q; want false circuit_wait", due, reason)
	}
	if due, reason := accountRefreshDue(account, cfg, now.Add(2*time.Minute)); !due || reason != "circuit_probe_due" {
		t.Fatalf("accountRefreshDue at probe = %t, %q; want true circuit_probe_due", due, reason)
	}
}

func TestRecordRefreshAuthFailureStopsRetry(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	updated, ok := store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureAuth, "please re-login", now)
	if !ok {
		t.Fatal("RecordRefreshFailure returned ok=false")
	}
	if !updated.Refresh.AuthFailure {
		t.Fatal("AuthFailure = false, want true")
	}
	if !updated.Refresh.NextRetryAt.IsZero() {
		t.Fatalf("NextRetryAt = %s, want zero", updated.Refresh.NextRetryAt)
	}
}

func TestLocalRefreshFailureBlocksAutomaticDueRefresh(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})
	store.RecordRefreshFailure("auth-1", "idx-1", RefreshFailureLocal, "missing access token", now)

	due := store.DueAccounts(now.Add(24 * time.Hour))
	if len(due) != 0 {
		t.Fatalf("due accounts after local failure = %#v, want none", due)
	}
	account := store.Snapshot(now).Accounts[0]
	if ok, reason := accountRefreshDue(account, store.Config(), now); ok || reason != "local_failure" {
		t.Fatalf("accountRefreshDue = %t, %q; want false local_failure", ok, reason)
	}
}

func TestAccountRefreshDueReasons(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	if due, reason := accountRefreshDue(AccountState{AuthID: "a"}, cfg, now); !due || reason != "never_refreshed" {
		t.Fatalf("never refreshed due=%v reason=%q, want true never_refreshed", due, reason)
	}
	stale := AccountState{AuthID: "a", LastSuccessAt: now.Add(-6 * time.Hour)}
	if due, reason := accountRefreshDue(stale, cfg, now); !due || reason != "stale" {
		t.Fatalf("stale due=%v reason=%q, want true stale", due, reason)
	}
	retry := AccountState{AuthID: "a", LastSuccessAt: now.Add(-time.Hour)}
	retry.Refresh.NextRetryAt = now.Add(-time.Second)
	if due, reason := accountRefreshDue(retry, cfg, now); !due || reason != "retry_due" {
		t.Fatalf("retry due=%v reason=%q, want true retry_due", due, reason)
	}
	authFailed := AccountState{AuthID: "a", LastSuccessAt: now.Add(-6 * time.Hour)}
	authFailed.Refresh.AuthFailure = true
	if due, reason := accountRefreshDue(authFailed, cfg, now); due || reason != "auth_failure" {
		t.Fatalf("auth failure due=%v reason=%q, want false auth_failure", due, reason)
	}
}

func TestPluginStateDueAccountsAnnotatesReason(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.UpsertQuota(AccountState{AuthID: "fresh", LastSuccessAt: now})
	store.UpsertQuota(AccountState{AuthID: "stale", LastSuccessAt: now.Add(-6 * time.Hour)})

	due := store.DueAccounts(now)
	if len(due) != 1 {
		t.Fatalf("due accounts len = %d, want 1: %#v", len(due), due)
	}
	if due[0].AuthID != "stale" || due[0].Refresh.DueReason != "stale" {
		t.Fatalf("due account = %#v, want stale with reason", due[0])
	}
}
