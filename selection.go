package main

import (
	"sort"
	"time"
)

type AvailabilityClass uint8

const (
	Preferred AvailabilityClass = iota
	Opportunistic
	Excluded
)

type CacheClass uint8

const (
	CacheFresh CacheClass = iota
	CacheAging
	CacheUnknown
	CacheStale
)

type TrialState uint8

const (
	TrialNone TrialState = iota
	TrialActive
	TrialUnknown
)

type CircuitClass uint8

const (
	CircuitClosed CircuitClass = iota
	CircuitOpen
	CircuitHalfOpen
)

type AccountView struct {
	ID                   string
	Instance             AuthInstanceID
	PluginPriority       int
	Family               AccountFamily
	Cache                CacheClass
	LastKnownAvailable   bool
	Exhausted            bool
	ResetAt              time.Time
	AuthBlocked          bool
	CircuitOpen          bool
	TemporaryUnavailable bool
	Trial                TrialState
	Expiry               time.Time
	RemainingQuota       float64
}

type Candidate struct{ ID, Provider string }

type SelectionResult struct {
	AuthID   string
	Instance AuthInstanceID
	Class    AvailabilityClass
	Trial    bool
	Reason   string
	Ordered  []AccountView
}

func ClassifyAccount(a AccountView, now time.Time) AvailabilityClass {
	if a.AuthBlocked || a.CircuitOpen || a.TemporaryUnavailable || a.Trial != TrialNone {
		return Excluded
	}
	if a.Exhausted && a.ResetAt.After(now) {
		return Excluded
	}
	if a.Cache == CacheFresh || a.Cache == CacheAging {
		if !a.Exhausted {
			return Preferred
		}
		return Opportunistic
	}
	if a.Cache == CacheUnknown || (a.Cache == CacheStale && a.LastKnownAvailable) {
		return Opportunistic
	}
	return Excluded
}

func SelectAccount(snapshot SchedulerSnapshot, candidates []Candidate, now time.Time) SelectionResult {
	eligible := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		if c.ID != "" && c.Provider == "codex" {
			if _, ok := snapshot.ActiveHighestTier[c.ID]; ok {
				eligible[c.ID] = struct{}{}
			}
		}
	}
	byClass := map[AvailabilityClass][]AccountView{Preferred: {}, Opportunistic: {}}
	for _, a := range snapshot.Accounts {
		if _, ok := eligible[a.ID]; !ok {
			continue
		}
		class := ClassifyAccount(a, now)
		if class != Excluded {
			byClass[class] = append(byClass[class], a)
		}
	}
	for _, class := range []AvailabilityClass{Preferred, Opportunistic} {
		accounts := byClass[class]
		sort.Slice(accounts, func(i, j int) bool { return accountViewLess(accounts[i], accounts[j], snapshot.MonthlyMode) })
		if len(accounts) > 0 {
			return SelectionResult{AuthID: accounts[0].ID, Instance: accounts[0].Instance, Class: class, Trial: class == Opportunistic, Reason: "selected", Ordered: accounts}
		}
	}
	return SelectionResult{Reason: "no_selectable_account"}
}

func accountViewLess(a, b AccountView, mode MonthlyMode) bool {
	if a.PluginPriority != b.PluginPriority {
		return a.PluginPriority > b.PluginPriority
	}
	if mode == MonthlyModePriority && a.Family != b.Family {
		return a.Family == AccountFamilyMonthly
	}
	if !a.Expiry.Equal(b.Expiry) {
		if a.Expiry.IsZero() {
			return false
		}
		if b.Expiry.IsZero() {
			return true
		}
		return a.Expiry.Before(b.Expiry)
	}
	if a.RemainingQuota != b.RemainingQuota {
		return a.RemainingQuota > b.RemainingQuota
	}
	return a.ID < b.ID
}
