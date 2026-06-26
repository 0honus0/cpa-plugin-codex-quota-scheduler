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
