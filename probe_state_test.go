package main

import (
	"reflect"
	"testing"
	"time"
)

func TestProbeControllerPersistentStateSetAndIllegalNoop(t *testing.T) { //inv:INV-17,INV-19
	now := time.Unix(100, 0).UTC()
	c := NewProbeController(now)
	c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbeIdle})
	before, _ := c.Window(1, ProbeWindowFiveHour)
	if intents := c.Advance(1, ProbeEvent{Kind: ProbeEventVerifyResult, Window: ProbeWindowFiveHour, Now: now}); len(intents) != 0 {
		t.Fatalf("illegal intents=%v", intents)
	}
	after, _ := c.Window(1, ProbeWindowFiveHour)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("illegal mutation: %#v -> %#v", before, after)
	}
}

func TestProbeControllerDualWindowIndependent(t *testing.T) { //inv:INV-17
	now := time.Unix(1000, 0).UTC()
	c := NewProbeController(now)
	c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 80, 5*time.Hour)})
	c.SetWindow(1, ProbeWindowLong, ProbeWindow{State: ProbePendingCheck, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 60, 7*24*time.Hour)})
	ints := c.Advance(1, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{
		ProbeWindowFiveHour: {Valid: true, ResetAt: ptrTime(now.Add(4 * time.Hour)), Usage: ptrFloat(0)},
		ProbeWindowLong:     {Valid: true, ResetAt: ptrTime(now.Add(-time.Hour)), Usage: ptrFloat(60)},
	}})
	five, _ := c.Window(1, ProbeWindowFiveHour)
	long, _ := c.Window(1, ProbeWindowLong)
	if five.State != ProbeConfirmed || long.State != ProbeSentAwaitingVerify {
		t.Fatalf("five=%s long=%s intents=%v", five.State, long.State, ints)
	}
}

func TestProbeControllerDormantDeadlineStillEmitsProbe(t *testing.T) { //inv:INV-14,INV-32
	now := time.Unix(2000, 0).UTC()
	c := NewProbeController(now)
	c.SetWindow(2, ProbeWindowFiveHour, ProbeWindow{State: ProbeWaitingReset, Deadline: now})
	ints := c.Advance(2, ProbeEvent{Kind: ProbeEventDeadline, Window: ProbeWindowFiveHour, Now: now, RefreshMode: RefreshModeDormant})
	if len(ints) != 1 || ints[0].Class != OperationProbeRead {
		t.Fatalf("intents=%v", ints)
	}
}

func TestMockGroupC(t *testing.T) {
	t.Run("classifier", TestProbeClassifierOrderedRules)
	t.Run("dual", TestProbeControllerDualWindowIndependent)
}

func TestProbeAllStateEventsAndDualWindowProduct(t *testing.T) { //inv:INV-17,INV-18,INV-19
	states := []ProbeWindowState{ProbeIdle, ProbeWaitingReset, ProbePendingCheck, ProbeSentAwaitingVerify, ProbeSentUnknown, ProbeRetryWait, ProbeConfirmed, ProbeAuthBlocked, ProbeAnomalyHold, ProbeWaitingRoster}
	events := []ProbeEventKind{ProbeEventDeadline, ProbeEventPrecheckResult, ProbeEventVerifyResult, ProbeEventAuthFailed, ProbeEventExternalLogin, ProbeEventRosterConfirmed, ProbeEventInstanceRemoved}
	now := time.Unix(9000, 0).UTC()
	for _, left := range states {
		for _, right := range states {
			c := NewProbeController(now)
			base := ResetProbeBaseline(now.Add(-time.Hour), 80, 5*time.Hour)
			c.SetWindow(1, ProbeWindowFiveHour, ProbeWindow{State: left, Baseline: base, Deadline: now})
			c.SetWindow(1, ProbeWindowLong, ProbeWindow{State: right, Baseline: base, Deadline: now})
			c.Advance(1, ProbeEvent{Kind: ProbeEventPrecheckResult, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{ProbeWindowFiveHour: {Valid: true, ResetAt: ptrTime(now.Add(-time.Hour)), Usage: ptrFloat(80)}, ProbeWindowLong: {Valid: true, ResetAt: ptrTime(now.Add(time.Hour)), Usage: ptrFloat(0)}}})
			for _, k := range []ProbeWindowKind{ProbeWindowFiveHour, ProbeWindowLong} {
				if w, ok := c.Window(1, k); ok && !validProbeState(w.State) {
					t.Fatalf("%s x %s produced %q", left, right, w.State)
				}
			}
		}
	}
	for _, s := range states {
		for _, e := range events {
			c := NewProbeController(now)
			c.SetWindow(2, ProbeWindowFiveHour, ProbeWindow{State: s, Baseline: ResetProbeBaseline(now.Add(-time.Hour), 80, 0), Deadline: now})
			c.Advance(2, ProbeEvent{Kind: e, Window: ProbeWindowFiveHour, Now: now, Snapshots: map[ProbeWindowKind]QuotaSnapshot{ProbeWindowFiveHour: {Valid: true, ResetAt: ptrTime(now.Add(-time.Hour)), Usage: ptrFloat(80)}}})
			if w, ok := c.Window(2, ProbeWindowFiveHour); ok && !validProbeState(w.State) {
				t.Fatalf("state=%s event=%s -> %q", s, e, w.State)
			}
		}
	}
}
func validProbeState(s ProbeWindowState) bool {
	switch s {
	case ProbeIdle, ProbeWaitingReset, ProbePendingCheck, ProbeSentAwaitingVerify, ProbeSentUnknown, ProbeRetryWait, ProbeConfirmed, ProbeAuthBlocked, ProbeAnomalyHold, ProbeWaitingRoster:
		return true
	}
	return false
}
func TestSuiteProbe(t *testing.T) {
	t.Run("state", TestProbeControllerPersistentStateSetAndIllegalNoop)
	t.Run("dormant", TestProbeControllerDormantDeadlineStillEmitsProbe)
}
