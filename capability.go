package main

import (
	"context"
	"sort"
	"time"
)

type HostCapability uint8

const (
	CapabilityB HostCapability = iota
	CapabilityA
)

// RosterEntry is the normalized host.auth.list boundary shape. Priority is a
// pointer because the v7.2.42 typed ABI uses int and cannot distinguish a
// missing JSON field from an explicit zero.
type RosterEntry struct {
	ID        string
	AuthIndex string
	Provider  string
	Priority  *int
}

type HostRosterSnapshot struct {
	Capability  HostCapability
	Entries     []RosterEntry
	ConfirmedAt time.Time
}

type HostAuthLister interface {
	ListHostAuths(context.Context) ([]RosterEntry, error)
}

type HostAuthListerFunc func(context.Context) ([]RosterEntry, error)

func (f HostAuthListerFunc) ListHostAuths(ctx context.Context) ([]RosterEntry, error) {
	return f(ctx)
}

func DetectHostRoster(ctx context.Context, host HostAuthLister, now time.Time) HostRosterSnapshot {
	if host == nil {
		return HostRosterSnapshot{Capability: CapabilityB}
	}
	entries, err := host.ListHostAuths(ctx)
	if err != nil || len(entries) == 0 {
		return HostRosterSnapshot{Capability: CapabilityB}
	}
	for _, entry := range entries {
		if entry.Provider == "codex" && entry.Priority == nil {
			return HostRosterSnapshot{Capability: CapabilityB}
		}
	}
	return HostRosterSnapshot{
		Capability:  CapabilityA,
		Entries:     append([]RosterEntry(nil), entries...),
		ConfirmedAt: now,
	}
}

func HighestCodexTier(entries []RosterEntry) (priority int, ids []string, ok bool) {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.ID == "" || entry.Provider != "codex" || entry.Priority == nil {
			continue
		}
		if !ok || *entry.Priority > priority {
			priority = *entry.Priority
			ids = ids[:0]
			clear(seen)
			ok = true
		}
		if *entry.Priority == priority {
			if _, exists := seen[entry.ID]; !exists {
				seen[entry.ID] = struct{}{}
				ids = append(ids, entry.ID)
			}
		}
	}
	if !ok {
		return 0, nil, false
	}
	sort.Strings(ids)
	return priority, ids, true
}
