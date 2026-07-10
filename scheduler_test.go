package main

import (
	"strings"
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

func TestHighestPriorityCodexAdmissionKeepsOnlyMaximumTier(t *testing.T) {
	req := pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "high-a", Provider: "codex", Priority: 1},
		{ID: "low", Provider: "codex", Priority: 0},
		{ID: "high-b", Provider: "codex", Priority: 1},
		{ID: "other", Provider: "openai", Priority: 99},
	}}
	admission, ok := HighestPriorityCodexAdmission(req)
	if !ok || !admission.Observed || admission.Priority != 1 || len(admission.AuthIDs) != 2 {
		t.Fatalf("admission = %#v, ok=%t", admission, ok)
	}
	if _, ok := admission.AuthIDs["low"]; ok {
		t.Fatal("low CPA tier was admitted")
	}
}

func TestHighestPriorityCodexAdmissionRejectsNoCodexCandidates(t *testing.T) {
	admission, ok := HighestPriorityCodexAdmission(pluginapi.SchedulerPickRequest{
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "other", Provider: "openai", Priority: 9}},
	})
	if ok || admission.Observed {
		t.Fatalf("admission = %#v, ok=%t", admission, ok)
	}
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

func TestPickSkipsAuthFailedAccount(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	blocked := weeklyAccount("blocked", 5, now.Add(time.Hour), false)
	blocked.Refresh.AuthFailure = true
	available := weeklyAccount("available", 5, now.Add(2*time.Hour), false)
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{blocked, available}}

	decision := PickCodexAccount(requestWithCandidates("blocked", "available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available; ordered=%#v", decision.AuthID, decision.Ordered)
	}
	if decision.Ordered[1].Available || decision.Ordered[1].UnavailableReason != "auth_failure" {
		t.Fatalf("blocked account = %#v, want unavailable auth_failure", decision.Ordered[1])
	}
}

func TestPickSkipsLocalRefreshFailureAccount(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	blocked := weeklyAccount("blocked", 5, now.Add(time.Hour), false)
	blocked.Refresh.LastFailureKind = RefreshFailureLocal
	available := weeklyAccount("available", 5, now.Add(2*time.Hour), false)
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{blocked, available}}

	decision := PickCodexAccount(requestWithCandidates("blocked", "available"), snapshot, now)
	if decision.AuthID != "available" {
		t.Fatalf("AuthID = %q, want available; ordered=%#v", decision.AuthID, decision.Ordered)
	}
	if decision.Ordered[1].Available || decision.Ordered[1].UnavailableReason != "local_failure" {
		t.Fatalf("blocked account = %#v, want unavailable local_failure", decision.Ordered[1])
	}
}

func TestOrderedAccountsPutAvailableBeforeExhaustedWithinPriority(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	blocked := weeklyAccount("blocked", 5, now.Add(time.Hour), true)
	blocked.Quota.FiveHour.ResetAt = now.Add(time.Hour)
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		blocked,
		weeklyAccount("available", 5, now.Add(48*time.Hour), false),
	}}

	ordered := BuildOrderedAccounts(requestWithCandidates("blocked", "available"), snapshot, now)
	if len(ordered) != 2 || ordered[0].AuthID != "available" || !ordered[0].Available {
		t.Fatalf("ordered = %#v, want available account first", ordered)
	}
}

func TestOrderedAccountsSeparateAvailableTemporaryAndLongExhausted(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	tempLater := weeklyAccount("temp-later", 5, now.Add(24*time.Hour), true)
	tempLater.Quota.FiveHour.ResetAt = now.Add(4 * time.Hour)
	tempEarlier := weeklyAccount("temp-earlier", 5, now.Add(72*time.Hour), true)
	tempEarlier.Quota.FiveHour.ResetAt = now.Add(2 * time.Hour)
	longEarlier := weeklyAccount("long-earlier", 5, now.Add(12*time.Hour), false)
	longEarlier.Quota.LongWindow.Exhausted = true
	longLater := monthlyAccount("long-later", 5, now.Add(48*time.Hour))
	longLater.Quota.LongWindow.Exhausted = true
	availableLater := weeklyAccount("available-later", 5, now.Add(36*time.Hour), false)
	availableSoon := weeklyAccount("available-soon", 5, now.Add(6*time.Hour), false)
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		tempLater,
		longLater,
		availableLater,
		longEarlier,
		tempEarlier,
		availableSoon,
	}}

	ordered := BuildOrderedAccounts(requestWithCandidates("temp-later", "long-later", "available-later", "long-earlier", "temp-earlier", "available-soon"), snapshot, now)
	got := make([]string, 0, len(ordered))
	gotStatus := make([]string, 0, len(ordered))
	for _, account := range ordered {
		got = append(got, account.AuthID)
		gotStatus = append(gotStatus, string(account.QueueStatus))
	}
	want := []string{"available-soon", "available-later", "temp-earlier", "temp-later", "long-earlier", "long-later"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ordered = %#v, want %#v; statuses=%#v", got, want, gotStatus)
	}
	wantStatus := []string{"available", "available", "five_hour_exhausted", "five_hour_exhausted", "long_window_exhausted", "long_window_exhausted"}
	if strings.Join(gotStatus, ",") != strings.Join(wantStatus, ",") {
		t.Fatalf("statuses = %#v, want %#v", gotStatus, wantStatus)
	}
}

func TestOrderedAccountsMonthlyPriorityOnlyMovesAvailableMonthlyAhead(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.MonthlyMode = MonthlyModePriority
	blockedMonthly := monthlyAccount("blocked-monthly", 5, now.Add(time.Hour))
	blockedMonthly.Quota.LongWindow.Exhausted = true
	availableWeekly := weeklyAccount("available-weekly", 5, now.Add(72*time.Hour), false)
	availableMonthly := monthlyAccount("available-monthly", 5, now.Add(96*time.Hour))
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		blockedMonthly,
		availableWeekly,
		availableMonthly,
	}}

	ordered := BuildOrderedAccounts(requestWithCandidates("blocked-monthly", "available-weekly", "available-monthly"), snapshot, now)
	got := []string{ordered[0].AuthID, ordered[1].AuthID, ordered[2].AuthID}
	want := []string{"available-monthly", "available-weekly", "blocked-monthly"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ordered = %#v, want %#v", got, want)
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

func TestPickCodexRequestWithNoCodexCandidatesReturnsUnhandled(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.Fallback = FallbackFillFirst
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		weeklyAccount("available", 5, now.Add(time.Hour), false),
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider:  "codex",
		Providers: []string{"codex"},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "not-codex", Provider: "openai", Priority: 10, Status: "active"},
		},
	}

	decision := PickCodexAccount(req, snapshot, now)
	if decision.Handled {
		t.Fatalf("Handled = true, want false; decision=%#v", decision)
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

func TestPickDoesNotScanBelowActivePriorityTier(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.Fallback = FallbackFillFirst
	highStale := weeklyAccount("high-stale", 10, now.Add(time.Hour), false)
	highStale.Stale = true
	lowAvailable := weeklyAccount("low-available", 1, now.Add(2*time.Hour), false)
	snapshot := StateSnapshot{Config: cfg, Now: now, Accounts: []AccountState{
		highStale,
		lowAvailable,
	}}
	req := pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "high-stale", Provider: "codex", Priority: 10, Status: "active"},
			{ID: "low-available", Provider: "codex", Priority: 1, Status: "active"},
		},
	}

	decision := PickCodexAccount(req, snapshot, now)
	if decision.AuthID != "" || !decision.Handled || decision.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("decision = %#v, want fill-first fallback without selecting lower CPA priority", decision)
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

func TestPickSkipsMonthlyWhenFiveHourExhausted(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	blocked := monthlyAccount("blocked", 5, now.Add(24*time.Hour))
	used := 100.0
	blocked.Quota.FiveHour = &QuotaWindow{
		Kind:        WindowFiveHour,
		UsedPercent: &used,
		ResetAt:     now.Add(time.Hour),
		Exhausted:   true,
	}
	available := monthlyAccount("available", 5, now.Add(48*time.Hour))
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{
		blocked,
		available,
	}}

	ordered := BuildOrderedAccounts(requestWithCandidates("blocked", "available"), snapshot, now)
	if len(ordered) != 2 {
		t.Fatalf("ordered = %#v", ordered)
	}
	if ordered[0].AuthID != "available" || !ordered[0].Available {
		t.Fatalf("first account = %#v, want available monthly account", ordered[0])
	}
	if ordered[1].AuthID != "blocked" || ordered[1].QueueStatus != QueueStatusFiveHourExhausted || ordered[1].UnavailableReason != "five_hour_exhausted" {
		t.Fatalf("blocked account = %#v, want monthly five-hour exhausted", ordered[1])
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

func TestPickSkipsOpenCircuitButAllowsHalfOpen(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	open := weeklyAccount("open", 5, now.Add(time.Hour), false)
	open.Circuit = CircuitBreakerState{
		State:        CircuitStateOpen,
		FailureCount: 3,
		OpenedAt:     now.Add(-time.Minute),
		NextProbeAt:  now.Add(9 * time.Minute),
		Reason:       usageLimitReachedReason,
	}
	halfOpen := weeklyAccount("half-open", 5, now.Add(2*time.Hour), false)
	halfOpen.Circuit = CircuitBreakerState{
		State:        CircuitStateOpen,
		FailureCount: 3,
		OpenedAt:     now.Add(-15 * time.Minute),
		NextProbeAt:  now.Add(-time.Minute),
		Reason:       usageLimitReachedReason,
	}
	snapshot := StateSnapshot{Config: DefaultConfig(), Now: now, Accounts: []AccountState{open, halfOpen}}

	ordered := BuildOrderedAccounts(requestWithCandidates("open", "half-open"), snapshot, now)
	if len(ordered) != 2 {
		t.Fatalf("ordered = %#v", ordered)
	}
	if ordered[0].AuthID != "half-open" || !ordered[0].Available || ordered[0].QueueStatus != QueueStatusAvailable {
		t.Fatalf("first ordered account = %#v, want half-open available", ordered[0])
	}
	if ordered[1].AuthID != "open" || ordered[1].Available || ordered[1].UnavailableReason != "circuit_open" {
		t.Fatalf("second ordered account = %#v, want open circuit unavailable", ordered[1])
	}
}
