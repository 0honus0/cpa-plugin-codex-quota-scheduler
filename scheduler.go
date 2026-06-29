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
	QueueStatus       QueueStatus
	Available         bool
	UnavailableReason string
	SortTime          time.Time
	Annotation        AccountAnnotation
}

type QueueStatus string

const (
	QueueStatusAvailable           QueueStatus = "available"
	QueueStatusFiveHourExhausted   QueueStatus = "five_hour_exhausted"
	QueueStatusLongWindowExhausted QueueStatus = "long_window_exhausted"
	QueueStatusUnavailable         QueueStatus = "unavailable"
)

func PickCodexAccount(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time) PickDecision {
	if !snapshot.Config.HandleEnabled {
		return PickDecision{Reason: "handle_disabled"}
	}
	if !requestIncludesCodex(req) {
		return PickDecision{Reason: "provider_not_codex"}
	}

	ordered := BuildOrderedAccounts(req, snapshot, now)
	if len(ordered) == 0 {
		return PickDecision{Reason: "no_codex_candidates", Ordered: ordered}
	}
	activePriority := ordered[0].Priority
	for _, account := range ordered {
		if account.Priority != activePriority {
			break
		}
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
				QueueStatus:       QueueStatusUnavailable,
				UnavailableReason: "unknown_account",
			})
			continue
		}

		queueStatus, available, reason, sortTime := accountQueueState(account, now)
		ordered = append(ordered, ScheduledAccount{
			AuthID:            candidate.ID,
			Priority:          candidate.Priority,
			Family:            account.Family,
			QueueStatus:       queueStatus,
			Available:         available,
			UnavailableReason: reason,
			SortTime:          sortTime,
			Annotation:        account.Annotation,
		})
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		left := ordered[i]
		right := ordered[j]
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		leftRank := queueStatusRank(left.QueueStatus)
		rightRank := queueStatusRank(right.QueueStatus)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.QueueStatus == QueueStatusAvailable && snapshot.Config.MonthlyMode == MonthlyModePriority && left.Family != right.Family {
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
	_, available, reason, _ := accountQueueState(account, now)
	return available, reason
}

func accountQueueState(account AccountState, now time.Time) (QueueStatus, bool, string, time.Time) {
	if account.Refresh.AuthFailure {
		return QueueStatusUnavailable, false, "auth_failure", time.Time{}
	}
	if account.Refresh.LastFailureKind == RefreshFailureLocal {
		return QueueStatusUnavailable, false, "local_failure", time.Time{}
	}
	if account.Stale {
		return QueueStatusUnavailable, false, "stale_quota", time.Time{}
	}
	circuit := effectiveCircuitState(account.Circuit, now)
	if circuit.EffectiveState == CircuitStateOpen {
		return QueueStatusUnavailable, false, "circuit_open", circuit.NextProbeAt
	}
	if circuit.EffectiveState == CircuitStateClosed && circuit.FailureCount > 0 && !circuit.NextProbeAt.IsZero() && circuit.NextProbeAt.After(now) {
		return QueueStatusUnavailable, false, "quota_probe_wait", circuit.NextProbeAt
	}
	if account.TemporaryExhausted && (account.TemporaryResetAt.IsZero() || account.TemporaryResetAt.After(now)) {
		return QueueStatusFiveHourExhausted, false, "temporary_exhausted", account.TemporaryResetAt
	}
	switch account.Family {
	case AccountFamilyWeekly:
		if account.Quota.FiveHour == nil {
			return QueueStatusUnavailable, false, "missing_five_hour_window", time.Time{}
		}
		if account.Quota.FiveHour.ResetAt.IsZero() {
			return QueueStatusUnavailable, false, "missing_five_hour_reset", time.Time{}
		}
		if account.Quota.LongWindow == nil {
			return QueueStatusUnavailable, false, "missing_weekly_window", time.Time{}
		}
		if account.Quota.LongWindow.ResetAt.IsZero() {
			return QueueStatusUnavailable, false, "missing_weekly_reset", time.Time{}
		}
		if windowExhausted(account.Quota.LongWindow, now) {
			return QueueStatusLongWindowExhausted, false, "weekly_exhausted", account.Quota.LongWindow.ResetAt
		}
		if windowExhausted(account.Quota.FiveHour, now) {
			return QueueStatusFiveHourExhausted, false, "five_hour_exhausted", account.Quota.FiveHour.ResetAt
		}
		return QueueStatusAvailable, true, "", account.Quota.LongWindow.ResetAt
	case AccountFamilyMonthly:
		if account.Quota.LongWindow == nil {
			return QueueStatusUnavailable, false, "missing_monthly_window", time.Time{}
		}
		if account.Quota.LongWindow.ResetAt.IsZero() {
			return QueueStatusUnavailable, false, "missing_monthly_reset", time.Time{}
		}
		if windowExhausted(account.Quota.LongWindow, now) {
			return QueueStatusLongWindowExhausted, false, "monthly_exhausted", account.Quota.LongWindow.ResetAt
		}
		if windowExhausted(account.Quota.FiveHour, now) {
			return QueueStatusFiveHourExhausted, false, "five_hour_exhausted", account.Quota.FiveHour.ResetAt
		}
		return QueueStatusAvailable, true, "", account.Quota.LongWindow.ResetAt
	default:
		return QueueStatusUnavailable, false, "unknown_family", time.Time{}
	}
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

func queueStatusRank(status QueueStatus) int {
	switch status {
	case QueueStatusAvailable:
		return 0
	case QueueStatusFiveHourExhausted:
		return 1
	case QueueStatusLongWindowExhausted:
		return 2
	default:
		return 3
	}
}

func windowExhausted(window *QuotaWindow, now time.Time) bool {
	if window == nil || !window.Exhausted {
		return false
	}
	return window.ResetAt.IsZero() || window.ResetAt.After(now)
}
