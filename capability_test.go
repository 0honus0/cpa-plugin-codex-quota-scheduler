package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

type fakeHostAuthLister struct {
	entries []RosterEntry
	err     error
	calls   int
}

func (f *fakeHostAuthLister) ListHostAuths(context.Context) ([]RosterEntry, error) {
	f.calls++
	return append([]RosterEntry(nil), f.entries...), f.err
}

func priority(value int) *int { return &value }

func TestSuiteCapabilityDetectsExplicitPriorityRosterOnce(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	host := &fakeHostAuthLister{entries: []RosterEntry{
		{ID: "codex-a", Provider: "codex", Priority: priority(7)},
		{ID: "other", Provider: "openai", Priority: priority(99)},
	}}

	snapshot := DetectHostRoster(context.Background(), host, now)

	if host.calls != 1 { // INV-31: one detection performs one host roster call.
		t.Fatalf("host calls = %d, want 1", host.calls)
	}
	if snapshot.Capability != CapabilityA {
		t.Fatalf("capability = %v, want CapabilityA", snapshot.Capability)
	}
	if !snapshot.ConfirmedAt.Equal(now) {
		t.Fatalf("confirmed_at = %s, want %s", snapshot.ConfirmedAt, now)
	}
	if !reflect.DeepEqual(snapshot.Entries, host.entries) {
		t.Fatalf("entries = %#v, want %#v", snapshot.Entries, host.entries)
	}
}

func TestSuiteCapabilityFallsBackOnHostFailure(t *testing.T) {
	host := &fakeHostAuthLister{err: errors.New("host unavailable")}
	snapshot := DetectHostRoster(context.Background(), host, time.Now())
	if snapshot.Capability != CapabilityB || len(snapshot.Entries) != 0 || !snapshot.ConfirmedAt.IsZero() {
		t.Fatalf("snapshot = %#v, want unconfirmed CapabilityB", snapshot)
	}
}

func TestSuiteCapabilityFallsBackWhenPriorityIsMissing(t *testing.T) {
	host := &fakeHostAuthLister{entries: []RosterEntry{{ID: "codex-a", Provider: "codex"}}}
	snapshot := DetectHostRoster(context.Background(), host, time.Now())
	if snapshot.Capability != CapabilityB || len(snapshot.Entries) != 0 || !snapshot.ConfirmedAt.IsZero() {
		t.Fatalf("snapshot = %#v, want unconfirmed CapabilityB", snapshot)
	}
}

func TestSuiteCapabilityFallsBackForEmptyRoster(t *testing.T) {
	host := &fakeHostAuthLister{}
	snapshot := DetectHostRoster(context.Background(), host, time.Now())
	if snapshot.Capability != CapabilityB || !snapshot.ConfirmedAt.IsZero() {
		t.Fatalf("snapshot = %#v, want unconfirmed CapabilityB", snapshot)
	}
}

func TestSuiteCapabilityABINormalizationPreservesMissingPriority(t *testing.T) {
	lister := ABIHostAuthLister{call: func(string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"files":[{"id":"explicit-zero","provider":"codex","priority":0},{"id":"missing","provider":"codex"}]}`), nil
	}}

	entries, err := lister.ListHostAuths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Priority == nil || *entries[0].Priority != 0 || entries[1].Priority != nil {
		t.Fatalf("entries = %#v, want explicit zero followed by missing priority", entries)
	}
}

func TestSuiteCapabilityRawRosterPayloadsCannotProveCapability(t *testing.T) {
	tests := []struct {
		name    string
		payload json.RawMessage
	}{
		{name: "empty object", payload: json.RawMessage(`{}`)},
		{name: "missing files", payload: json.RawMessage(`{"other":[]}`)},
		{name: "null files", payload: json.RawMessage(`{"files":null}`)},
		{name: "empty files", payload: json.RawMessage(`{"files":[]}`)},
		{name: "malformed", payload: json.RawMessage(`{"files":`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := ABIHostAuthLister{call: func(string, any) (json.RawMessage, error) {
				return tt.payload, nil
			}}
			snapshot := DetectHostRoster(context.Background(), lister, time.Now())
			if snapshot.Capability != CapabilityB || len(snapshot.Entries) != 0 || !snapshot.ConfirmedAt.IsZero() {
				t.Fatalf("snapshot = %#v, want unconfirmed CapabilityB", snapshot)
			}
		})
	}
}

func TestSuiteCapabilityIgnoresMissingPriorityOnIrrelevantProvider(t *testing.T) {
	host := &fakeHostAuthLister{entries: []RosterEntry{
		{ID: "codex", Provider: "codex", Priority: priority(0)},
		{ID: "other", Provider: "openai"},
	}}
	snapshot := DetectHostRoster(context.Background(), host, time.Now())
	if snapshot.Capability != CapabilityA {
		t.Fatalf("capability = %v, want CapabilityA", snapshot.Capability)
	}
}

func TestSuiteCapabilityStartupTimeoutPublishesFallbackWithoutRealSleep(t *testing.T) {
	timer := make(chan time.Time)
	release := make(chan struct{})
	host := HostAuthListerFunc(func(context.Context) ([]RosterEntry, error) {
		<-release
		return []RosterEntry{{ID: "codex", Provider: "codex", Priority: priority(1)}}, nil
	})
	result := make(chan HostRosterSnapshot, 1)
	go func() {
		result <- detectStartupHostRoster(context.Background(), host, time.Now(), func(delay time.Duration) <-chan time.Time {
			if delay != hostRosterDetectionTimeout {
				t.Errorf("timeout = %s, want %s", delay, hostRosterDetectionTimeout)
			}
			return timer
		})
	}()
	timer <- time.Now()
	snapshot := <-result
	close(release)
	if snapshot.Capability != CapabilityB || !snapshot.ConfirmedAt.IsZero() {
		t.Fatalf("snapshot = %#v, want timeout CapabilityB", snapshot)
	}
}

func TestSuiteCapabilityHighestCodexTierFiltersProviders(t *testing.T) {
	gotPriority, gotIDs, ok := HighestCodexTier([]RosterEntry{
		{ID: "other", Provider: "openai", Priority: priority(100)},
		{ID: "codex", Provider: "codex", Priority: priority(3)},
	})
	if !ok || gotPriority != 3 || !reflect.DeepEqual(gotIDs, []string{"codex"}) {
		t.Fatalf("HighestCodexTier = (%d, %#v, %t), want (3, [codex], true)", gotPriority, gotIDs, ok)
	}
}

func TestSuiteCapabilityHighestCodexTierKeepsEqualHighestAndExcludesLower(t *testing.T) {
	gotPriority, gotIDs, ok := HighestCodexTier([]RosterEntry{
		{ID: "high-b", Provider: "codex", Priority: priority(8)},
		{ID: "low", Provider: "codex", Priority: priority(7)},
		{ID: "high-a", Provider: "codex", Priority: priority(8)},
	})
	if !ok || gotPriority != 8 || !reflect.DeepEqual(gotIDs, []string{"high-a", "high-b"}) { // INV-34.
		t.Fatalf("HighestCodexTier = (%d, %#v, %t), want (8, [high-a high-b], true)", gotPriority, gotIDs, ok)
	}
}

func TestSuiteCapabilityHighestCodexTierRejectsEmptyResults(t *testing.T) {
	if priority, ids, ok := HighestCodexTier(nil); ok || priority != 0 || ids != nil {
		t.Fatalf("HighestCodexTier(nil) = (%d, %#v, %t), want zero values", priority, ids, ok)
	}
}
