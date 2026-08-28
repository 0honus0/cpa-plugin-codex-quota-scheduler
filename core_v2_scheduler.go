package main

import (
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type corePickCandidate struct {
	candidate pluginapi.SchedulerAuthCandidate
	account   CoreAccount
	remaining float64
}

func (e *CoreEngine) Pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	cfg := e.Config()
	if !cfg.HandleEnabled || !coreRequestIncludesCodex(req) {
		return pluginapi.SchedulerPickResponse{}
	}

	priority, ids, ok := coreHighestCandidateTier(req.Candidates)
	if !ok {
		return pluginapi.SchedulerPickResponse{}
	}
	now := e.now()
	accounts := make(map[string]CoreAccount)
	for _, account := range e.Accounts() {
		accounts[account.ID] = account
	}

	choices := make([]corePickCandidate, 0, len(ids))
	unknown := false
	for _, candidate := range req.Candidates {
		if candidate.Provider != "codex" || candidate.Priority != priority {
			continue
		}
		if _, allowed := ids[candidate.ID]; !allowed {
			continue
		}
		account, exists := accounts[candidate.ID]
		if !exists {
			unknown = true
			continue
		}
		if !account.Selectable(now, cfg.QuotaStaleAfter) {
			continue
		}
		choices = append(choices, corePickCandidate{candidate: candidate, account: account, remaining: coreRemainingQuota(account.Quota)})
	}

	if len(choices) == 0 {
		reason := "no_selectable_account_in_cpa_priority"
		if unknown {
			reason = "unknown_account_in_cpa_priority"
		}
		e.mu.Lock()
		e.lastReason = reason
		e.mu.Unlock()
		// CPA has already reduced candidates to its highest available priority tier.
		// Delegating here preserves CPA as the authority for cross-tier priority.
		return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst}
	}

	sort.SliceStable(choices, func(i, j int) bool {
		left, right := choices[i], choices[j]
		if left.account.Preference.SchedulerPriority != right.account.Preference.SchedulerPriority {
			return left.account.Preference.SchedulerPriority > right.account.Preference.SchedulerPriority
		}
		if left.remaining != right.remaining {
			return left.remaining > right.remaining
		}
		return left.account.ID < right.account.ID
	})
	selected := choices[0]
	e.mu.Lock()
	e.lastSelected = selected.account.ID
	e.lastReason = "selected_same_cpa_priority"
	e.mu.Unlock()
	e.recordLog("info", "scheduler.selected", "selected account inside CPA priority tier", map[string]any{
		"auth_id":            selected.account.ID,
		"cpa_priority":       priority,
		"scheduler_priority": selected.account.Preference.SchedulerPriority,
	})
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: selected.account.ID}
}

func coreHighestCandidateTier(candidates []pluginapi.SchedulerAuthCandidate) (int, map[string]struct{}, bool) {
	ids := make(map[string]struct{})
	priority := 0
	found := false
	for _, candidate := range candidates {
		if candidate.ID == "" || !strings.EqualFold(candidate.Provider, "codex") {
			continue
		}
		if !found || candidate.Priority > priority {
			found = true
			priority = candidate.Priority
			clear(ids)
		}
		if candidate.Priority == priority {
			ids[candidate.ID] = struct{}{}
		}
	}
	return priority, ids, found
}

func coreRequestIncludesCodex(req pluginapi.SchedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(req.Provider), "codex") {
		return true
	}
	for _, provider := range req.Providers {
		if strings.EqualFold(strings.TrimSpace(provider), "codex") {
			return true
		}
	}
	return false
}

func coreRemainingQuota(quota ParsedQuota) float64 {
	remaining := 0.0
	found := false
	for _, window := range []*QuotaWindow{quota.FiveHour, quota.LongWindow} {
		if window == nil || window.UsedPercent == nil {
			continue
		}
		value := 100 - *window.UsedPercent
		if value < 0 {
			value = 0
		}
		if !found || value < remaining {
			remaining = value
			found = true
		}
	}
	if !found {
		return 0
	}
	return remaining
}

func coreAccountUnavailableReason(account CoreAccount, cfg CoreConfig, now time.Time) string {
	if account.Disabled401 {
		return "disabled_401"
	}
	if account.Banned(now) {
		return "autoban_429"
	}
	if account.LastSuccessAt.IsZero() {
		return "quota_unknown"
	}
	if cfg.QuotaStaleAfter > 0 && now.Sub(account.LastSuccessAt) > cfg.QuotaStaleAfter {
		return "quota_stale"
	}
	if account.Quota.FiveHour != nil && coreWindowExhausted(account.Quota.FiveHour, now) {
		return "five_hour_exhausted"
	}
	if account.Quota.LongWindow != nil && coreWindowExhausted(account.Quota.LongWindow, now) {
		return "long_window_exhausted"
	}
	return ""
}
