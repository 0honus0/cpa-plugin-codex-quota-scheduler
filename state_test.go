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
