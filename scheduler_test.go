package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func weeklyAccount(id string, priority int, weeklyReset time.Time, fiveHourExhausted bool) AccountState {
	usedFive := 10.0
	if fiveHourExhausted {
		usedFive = 100
	}
	return AccountState{
		AuthID:   id,
		Provider: "codex",
		Priority: priority,
		Family:   AccountFamilyWeekly,
		Quota: ParsedQuota{
			Family:     AccountFamilyWeekly,
			FiveHour:   &QuotaWindow{Kind: WindowFiveHour, UsedPercent: &usedFive, ResetAt: weeklyReset.Add(-20 * time.Hour), Exhausted: fiveHourExhausted},
			LongWindow: &QuotaWindow{Kind: WindowWeekly, ResetAt: weeklyReset},
		},
		LastSuccessAt: weeklyReset.Add(-48 * time.Hour),
	}
}

func monthlyAccount(id string, priority int, monthlyReset time.Time) AccountState {
	used := 20.0
	return AccountState{
		AuthID:   id,
		Provider: "codex",
		Priority: priority,
		Family:   AccountFamilyMonthly,
		Quota: ParsedQuota{
			Family:     AccountFamilyMonthly,
			LongWindow: &QuotaWindow{Kind: WindowMonthly, UsedPercent: &used, ResetAt: monthlyReset},
		},
		LastSuccessAt: monthlyReset.Add(-48 * time.Hour),
	}
}

func requestWithCandidates(ids ...string) pluginapi.SchedulerPickRequest {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Status: "active"})
	}
	return pluginapi.SchedulerPickRequest{Provider: "codex", Providers: []string{"codex"}, Candidates: candidates}
}

func TestPickRespectsCPAPriorityBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModeExpiryOrder
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("low-earliest", 1, now.Add(1*time.Hour), false),
		weeklyAccount("high-later", 10, now.Add(72*time.Hour), false),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "low-earliest", Provider: "codex", Priority: 1, Status: "active"},
			{ID: "high-later", Provider: "codex", Priority: 10, Status: "active"},
		},
	}
	decision := PickCodexAccount(req, snapshot, now)
	if !decision.Handled || decision.AuthID != "high-later" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestPickWeeklyEarliestResetWithinPriority(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("later", 5, now.Add(48*time.Hour), false),
		weeklyAccount("earlier", 5, now.Add(24*time.Hour), false),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "later", Provider: "codex", Priority: 5, Status: "active"},
			{ID: "earlier", Provider: "codex", Priority: 5, Status: "active"},
		},
	}
	decision := PickCodexAccount(req, snapshot, now)
	if decision.AuthID != "earlier" {
		t.Fatalf("AuthID = %q, want earlier", decision.AuthID)
	}
}

func TestPickMonthlyPriorityMode(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModePriority
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("weekly-soon", 5, now.Add(1*time.Hour), false),
		monthlyAccount("monthly-later", 5, now.Add(72*time.Hour)),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "weekly-soon", Provider: "codex", Priority: 5, Status: "active"},
			{ID: "monthly-later", Provider: "codex", Priority: 5, Status: "active"},
		},
	}
	decision := PickCodexAccount(req, snapshot, now)
	if decision.AuthID != "monthly-later" {
		t.Fatalf("AuthID = %q, want monthly-later", decision.AuthID)
	}
}

func TestPickMonthlyExpiryOrderMode(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModeExpiryOrder
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("weekly-soon", 5, now.Add(1*time.Hour), false),
		monthlyAccount("monthly-later", 5, now.Add(72*time.Hour)),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "weekly-soon", Provider: "codex", Priority: 5, Status: "active"},
			{ID: "monthly-later", Provider: "codex", Priority: 5, Status: "active"},
		},
	}
	decision := PickCodexAccount(req, snapshot, now)
	if decision.AuthID != "weekly-soon" {
		t.Fatalf("AuthID = %q, want weekly-soon", decision.AuthID)
	}
}

func TestPickSkipsWeeklyWhenFiveHourExhausted(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	blocked := weeklyAccount("blocked", 5, now.Add(time.Hour), true)
	blocked.Quota.FiveHour.ResetAt = now.Add(time.Hour)
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		blocked,
		weeklyAccount("available", 5, now.Add(2*time.Hour), false),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "blocked", Provider: "codex", Priority: 5, Status: "active"},
			{ID: "available", Provider: "codex", Priority: 5, Status: "active"},
		},
	}
	decision := PickCodexAccount(req, snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available", decision.AuthID)
	}
}

func TestPickIgnoresNonCodexProvider(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		weeklyAccount("available", 5, now.Add(time.Hour), false),
	}}
	req := requestWithCandidates("available")
	req.Provider = "openai"
	req.Providers = []string{"openai"}

	decision := PickCodexAccount(req, snapshot, now)
	if decision.Handled {
		t.Fatalf("Handled = true, want false")
	}
}

func TestPickDisabledHandleReturnsUnhandled(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.HandleEnabled = false
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("available", 5, now.Add(time.Hour), false),
	}}

	decision := PickCodexAccount(requestWithCandidates("available"), snapshot, now)
	if decision.Handled {
		t.Fatalf("Handled = true, want false")
	}
}

func TestPickDelegatesFillFirstWhenNoSelectableAccount(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.Fallback = FallbackFillFirst
	blocked := weeklyAccount("blocked", 5, now.Add(time.Hour), true)
	blocked.Quota.FiveHour.ResetAt = now.Add(time.Hour)
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		blocked,
	}}

	decision := PickCodexAccount(requestWithCandidates("blocked"), snapshot, now)
	if !decision.Handled || decision.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestPickCandidatePriorityOverridesCachedPriority(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("cached-high", 100, now.Add(1*time.Hour), false),
		weeklyAccount("candidate-high", 1, now.Add(72*time.Hour), false),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "cached-high", Provider: "codex", Priority: 1, Status: "active"},
			{ID: "candidate-high", Provider: "codex", Priority: 10, Status: "active"},
		},
	}

	decision := PickCodexAccount(req, snapshot, now)
	if decision.AuthID != "candidate-high" {
		t.Fatalf("AuthID = %q, want candidate-high", decision.AuthID)
	}
}

func TestPickSkipsWeeklyWhenLongWindowExhausted(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	blocked := weeklyAccount("blocked", 5, now.Add(time.Hour), false)
	blocked.Quota.LongWindow.Exhausted = true
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		blocked,
		weeklyAccount("available", 5, now.Add(2*time.Hour), false),
	}}

	decision := PickCodexAccount(requestWithCandidates("blocked", "available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available", decision.AuthID)
	}
}

func TestPickSkipsMonthlyWhenLongWindowExhausted(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	blocked := monthlyAccount("blocked", 5, now.Add(time.Hour))
	blocked.Quota.LongWindow.Exhausted = true
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		blocked,
		monthlyAccount("available", 5, now.Add(2*time.Hour)),
	}}

	decision := PickCodexAccount(requestWithCandidates("blocked", "available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available", decision.AuthID)
	}
}

func TestPickAllowsWeeklyWhenExhaustedFiveHourResetPassed(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	account := weeklyAccount("available", 5, now.Add(24*time.Hour), true)
	account.Quota.FiveHour.ResetAt = now
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{account}}

	decision := PickCodexAccount(requestWithCandidates("available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available", decision.AuthID)
	}
}

func TestPickAllowsWeeklyWhenExhaustedLongResetPassed(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	account := weeklyAccount("available", 5, now, false)
	account.Quota.FiveHour.ResetAt = now.Add(5 * time.Hour)
	account.Quota.LongWindow.Exhausted = true
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{account}}

	decision := PickCodexAccount(requestWithCandidates("available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available", decision.AuthID)
	}
}

func TestPickAllowsMonthlyWhenExhaustedResetPassed(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	account := monthlyAccount("available", 5, now)
	account.Quota.LongWindow.Exhausted = true
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{account}}

	decision := PickCodexAccount(requestWithCandidates("available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available", decision.AuthID)
	}
}

func TestPickSkipsWeeklyWhenFiveHourResetMissing(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	blocked := weeklyAccount("blocked", 5, now.Add(time.Hour), false)
	blocked.Quota.FiveHour.ResetAt = time.Time{}
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		blocked,
		weeklyAccount("available", 5, now.Add(2*time.Hour), false),
	}}

	decision := PickCodexAccount(requestWithCandidates("blocked", "available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available", decision.AuthID)
	}
}

func TestPickSkipsWeeklyWhenLongResetMissing(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	blocked := weeklyAccount("blocked", 5, now.Add(time.Hour), false)
	blocked.Quota.LongWindow.ResetAt = time.Time{}
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		blocked,
		weeklyAccount("available", 5, now.Add(2*time.Hour), false),
	}}

	decision := PickCodexAccount(requestWithCandidates("blocked", "available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available", decision.AuthID)
	}
}

func TestPickDelegatesWhenMonthlyResetMissing(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	account := monthlyAccount("blocked", 5, now.Add(time.Hour))
	account.Quota.LongWindow.ResetAt = time.Time{}
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{account}}

	decision := PickCodexAccount(requestWithCandidates("blocked"), snapshot, now)
	if !decision.Handled || decision.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestPickSkipsStaleAccount(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	blocked := weeklyAccount("blocked", 5, now.Add(time.Hour), false)
	blocked.Stale = true
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		blocked,
		weeklyAccount("available", 5, now.Add(2*time.Hour), false),
	}}

	decision := PickCodexAccount(requestWithCandidates("blocked", "available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available", decision.AuthID)
	}
}
