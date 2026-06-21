package main

import (
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type PickDecision struct {
	AuthID          string
	Handled         bool
	DelegateBuiltin string
	Reason          string
	Ordered         []ScheduledAccount
}

type ScheduledAccount struct {
	AuthID            string
	Priority          int
	Family            AccountFamily
	Available         bool
	UnavailableReason string
	SortTime          time.Time
	Annotation        AccountAnnotation
}

func PickCodexAccount(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time) PickDecision {
	if !snapshot.Config.HandleEnabled {
		return PickDecision{Reason: "handle_disabled"}
	}
	if !requestIncludesCodex(req) {
		return PickDecision{Reason: "provider_not_codex"}
	}

	ordered := BuildOrderedAccounts(req, snapshot, now)
	for _, account := range ordered {
		if account.Available {
			return PickDecision{
				AuthID:  account.AuthID,
				Handled: true,
				Reason:  "selected",
				Ordered: ordered,
			}
		}
	}

	if snapshot.Config.Fallback == FallbackFillFirst {
		return PickDecision{
			Handled:         true,
			DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst,
			Reason:          "fallback_fill_first",
			Ordered:         ordered,
		}
	}
	return PickDecision{Reason: "no_selectable_account", Ordered: ordered}
}

func BuildOrderedAccounts(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time) []ScheduledAccount {
	accountsByAuthID := make(map[string]AccountState, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if account.AuthID == "" {
			continue
		}
		accountsByAuthID[account.AuthID] = account
	}

	ordered := make([]ScheduledAccount, 0, len(req.Candidates))
	seen := make(map[string]struct{}, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidate.ID == "" || candidate.Provider != "codex" {
			continue
		}
		if _, ok := seen[candidate.ID]; ok {
			continue
		}
		seen[candidate.ID] = struct{}{}

		account, ok := accountsByAuthID[candidate.ID]
		if !ok {
			ordered = append(ordered, ScheduledAccount{
				AuthID:            candidate.ID,
				Priority:          candidate.Priority,
				Family:            AccountFamilyUnknown,
				UnavailableReason: "unknown_account",
			})
			continue
		}

		available, reason := accountAvailable(account, now)
		ordered = append(ordered, ScheduledAccount{
			AuthID:            candidate.ID,
			Priority:          candidate.Priority,
			Family:            account.Family,
			Available:         available,
			UnavailableReason: reason,
			SortTime:          accountSortTime(account),
			Annotation:        account.Annotation,
		})
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		left := ordered[i]
		right := ordered[j]
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		if snapshot.Config.MonthlyMode == MonthlyModePriority && left.Family != right.Family {
			if left.Family == AccountFamilyMonthly {
				return true
			}
			if right.Family == AccountFamilyMonthly {
				return false
			}
		}
		if !left.SortTime.Equal(right.SortTime) {
			if left.SortTime.IsZero() {
				return false
			}
			if right.SortTime.IsZero() {
				return true
			}
			return left.SortTime.Before(right.SortTime)
		}
		return left.AuthID < right.AuthID
	})
	return ordered
}

func accountAvailable(account AccountState, now time.Time) (bool, string) {
	if account.Stale {
		return false, "stale_quota"
	}
	if account.TemporaryExhausted && (account.TemporaryResetAt.IsZero() || account.TemporaryResetAt.After(now)) {
		return false, "temporary_exhausted"
	}
	switch account.Family {
	case AccountFamilyWeekly:
		if account.Quota.FiveHour == nil {
			return false, "missing_five_hour_window"
		}
		if account.Quota.FiveHour.ResetAt.IsZero() {
			return false, "missing_five_hour_reset"
		}
		if windowExhausted(account.Quota.FiveHour, now) {
			return false, "five_hour_exhausted"
		}
		if account.Quota.LongWindow == nil {
			return false, "missing_weekly_window"
		}
		if account.Quota.LongWindow.ResetAt.IsZero() {
			return false, "missing_weekly_reset"
		}
		if windowExhausted(account.Quota.LongWindow, now) {
			return false, "weekly_exhausted"
		}
	case AccountFamilyMonthly:
		if account.Quota.LongWindow == nil {
			return false, "missing_monthly_window"
		}
		if account.Quota.LongWindow.ResetAt.IsZero() {
			return false, "missing_monthly_reset"
		}
		if windowExhausted(account.Quota.LongWindow, now) {
			return false, "monthly_exhausted"
		}
	default:
		return false, "unknown_family"
	}
	return true, ""
}

func requestIncludesCodex(req pluginapi.SchedulerPickRequest) bool {
	if req.Provider == "codex" {
		return true
	}
	for _, provider := range req.Providers {
		if provider == "codex" {
			return true
		}
	}
	return false
}

func accountSortTime(account AccountState) time.Time {
	if account.Quota.LongWindow != nil {
		return account.Quota.LongWindow.ResetAt
	}
	return time.Time{}
}

func windowExhausted(window *QuotaWindow, now time.Time) bool {
	if window == nil || !window.Exhausted {
		return false
	}
	return window.ResetAt.IsZero() || window.ResetAt.After(now)
}
