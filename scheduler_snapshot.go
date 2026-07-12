package main

import (
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type SchedulerSnapshot struct {
	HandleEnabled     bool
	Fallback          FallbackMode
	MonthlyMode       MonthlyMode
	Accounts          []AccountView
	ActiveHighestTier map[string]struct{}
	Trials            *TrialRegistry
	EvidenceIntents   chan<- AuthInstanceID
}

var publishedSchedulerSnapshot atomic.Pointer[SchedulerSnapshot]

func PublishSchedulerSnapshot(snapshot *SchedulerSnapshot) {
	if snapshot == nil {
		return
	}
	copy := cloneSchedulerSnapshot(*snapshot)
	publishedSchedulerSnapshot.Store(&copy)
}
func cloneSchedulerSnapshot(s SchedulerSnapshot) SchedulerSnapshot {
	s.Accounts = append([]AccountView(nil), s.Accounts...)
	s.ActiveHighestTier = cloneStringSet(s.ActiveHighestTier)
	return s
}
func cloneStringSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func schedulerPickPublished(req pluginapi.SchedulerPickRequest, now time.Time) PickDecision {
	snapshot := publishedSchedulerSnapshot.Load()
	if snapshot == nil || !snapshot.HandleEnabled {
		return PickDecision{Reason: "handle_disabled"}
	}
	if !requestIncludesCodex(req) {
		return PickDecision{Reason: "provider_not_codex"}
	}
	candidates := make([]Candidate, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		candidates = append(candidates, Candidate{ID: c.ID, Provider: c.Provider})
	}
	working := cloneSchedulerSnapshot(*snapshot)
	result := SelectAccount(working, candidates, now)
	for result.AuthID != "" && result.Class == Opportunistic && (snapshot.Trials == nil || !snapshot.Trials.TryBegin(result.Instance, now)) {
		for i := range working.Accounts {
			if working.Accounts[i].Instance == result.Instance {
				working.Accounts[i].Trial = TrialActive
			}
		}
		result = SelectAccount(working, candidates, now)
	}
	if result.AuthID != "" && result.Class == Opportunistic {
		select {
		case snapshot.EvidenceIntents <- result.Instance:
		default:
		}
	}
	if result.AuthID != "" {
		return PickDecision{AuthID: result.AuthID, Handled: true, Reason: "selected"}
	}
	if snapshot.Fallback == FallbackFillFirst {
		return PickDecision{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst, Reason: result.Reason}
	}
	return PickDecision{Reason: result.Reason}
}

func schedulerSnapshotFromState(state StateSnapshot, trials *TrialRegistry) *SchedulerSnapshot {
	active := cloneStringSet(state.CPAAdmission.AuthIDs)
	accounts := make([]AccountView, 0, len(state.Accounts))
	for _, a := range state.Accounts {
		cache := CacheFresh
		if a.LastSuccessAt.IsZero() {
			cache = CacheUnknown
		} else if a.Stale {
			cache = CacheStale
		} else if state.Now.Sub(a.LastSuccessAt) > state.Config.QuotaRefreshInterval {
			cache = CacheAging
		}
		exhausted, reset := accountExhaustion(a, state.Now)
		trial := TrialNone
		if trials != nil {
			trial = trials.State(a.Instance, state.Now)
		}
		accounts = append(accounts, AccountView{ID: a.AuthID, Instance: a.Instance, PluginPriority: a.Annotation.SchedulerPriority, Family: a.Family, Cache: cache, LastKnownAvailable: a.LastError == "", Exhausted: exhausted, ResetAt: reset, AuthBlocked: a.Refresh.AuthFailure, CircuitOpen: effectiveCircuitState(a.Circuit, state.Now).EffectiveState == CircuitStateOpen, TemporaryUnavailable: a.TemporaryExhausted && a.TemporaryResetAt.After(state.Now), Trial: trial, Expiry: accountSortTime(a), RemainingQuota: remainingQuota(a)})
	}
	return &SchedulerSnapshot{HandleEnabled: state.Config.HandleEnabled, Fallback: state.Config.Fallback, MonthlyMode: state.Config.MonthlyMode, Accounts: accounts, ActiveHighestTier: active, Trials: trials}
}

func publishSchedulerState(state *PluginState, active map[string]struct{}, now time.Time) {
	if state == nil {
		return
	}
	s := state.Snapshot(now)
	if active != nil {
		s.CPAAdmission = CPAAdmissionState{Observed: true, AuthIDs: cloneStringSet(active)}
	}
	PublishSchedulerSnapshot(schedulerSnapshotFromState(s, globalTrials))
}
func accountExhaustion(a AccountState, now time.Time) (bool, time.Time) {
	if windowExhausted(a.Quota.LongWindow, now) {
		return true, a.Quota.LongWindow.ResetAt
	}
	if windowExhausted(a.Quota.FiveHour, now) {
		return true, a.Quota.FiveHour.ResetAt
	}
	return false, time.Time{}
}
func remainingQuota(a AccountState) float64 {
	for _, w := range []*QuotaWindow{a.Quota.LongWindow, a.Quota.FiveHour} {
		if w != nil && w.UsedPercent != nil {
			return 100 - *w.UsedPercent
		}
	}
	return 0
}
