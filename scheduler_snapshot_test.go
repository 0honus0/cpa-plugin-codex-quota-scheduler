package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// inv:INV-43,INV-44
func TestSchedulerPickABIPathSnapshotOnly(t *testing.T) {
	now := time.Now()
	s := &SchedulerSnapshot{HandleEnabled: true, Accounts: []AccountView{{ID: "a", Instance: 1, Cache: CacheFresh}, {ID: "outside", Instance: 2, Cache: CacheFresh, PluginPriority: 99}}, ActiveHighestTier: map[string]struct{}{"a": {}}}
	PublishSchedulerSnapshot(s)
	before := globalState.CPAAdmission()
	raw, _ := json.Marshal(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "outside", Provider: "codex"}, {ID: "a", Provider: "codex"}}})
	encoded, err := handleSchedulerPick(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(encoded, &env); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.AuthID != "a" {
		t.Fatalf("selected %q", response.AuthID)
	}
	if after := globalState.CPAAdmission(); !equalCPAAdmission(before, after) {
		t.Fatalf("ABI pick mutated roster: before=%#v after=%#v", before, after)
	}
	_ = now
}

func TestProductionEvidenceQueueAndDynamicTrial(t *testing.T) {
	now := time.Now()
	trials := NewTrialRegistry()
	intents := make(chan EvidenceIntent, 1)
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Fallback: FallbackFillFirst, Trials: trials, EvidenceIntents: intents, Accounts: []AccountView{{ID: "a", Instance: 11, Cache: CacheUnknown}}, ActiveHighestTier: map[string]struct{}{"a": {}}})
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}}}
	if got := schedulerPickPublished(req, now); got.AuthID != "a" {
		t.Fatalf("first=%#v", got)
	}
	trials.Advance(11, now.Add(60*time.Second))
	if trials.State(11, now) != TrialActive {
		t.Fatal("enqueued but unconsumed evidence was not marked pending before pick returned")
	}
	select {
	case intent := <-intents:
		if intent.Instance != 11 || intent.AuthID != "a" {
			t.Fatalf("intent=%#v", intent)
		}
	default:
		t.Fatal("trial intent missing")
	}
	if got := schedulerPickPublished(req, now.Add(time.Second)); got.AuthID != "" {
		t.Fatalf("active trial selected again: %#v", got)
	}
	trials.ObserveEvidence(11, Evidence{Kind: EvidenceRequestSuccess, At: now.Add(2 * time.Second)})
	if got := schedulerPickPublished(req, now.Add(3*time.Second)); got.AuthID != "a" {
		t.Fatalf("evidence not immediately visible: %#v", got)
	}
}

func TestUsageSuccessHandlerClearsTrialButProbeSuccessDoesNot(t *testing.T) {
	now := time.Now()
	trials := NewTrialRegistry()
	intents := make(chan EvidenceIntent, 2)
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Trials: trials, EvidenceIntents: intents, Accounts: []AccountView{{ID: "success", Instance: 41, Cache: CacheUnknown}}, ActiveHighestTier: map[string]struct{}{"success": {}}})
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "success", Provider: "codex"}}}
	if got := schedulerPickPublished(req, now); got.AuthID != "success" {
		t.Fatalf("begin=%#v", got)
	}
	raw, _ := json.Marshal(pluginapi.UsageRecord{Provider: "codex", AuthID: "success", Failed: false})
	if _, err := handleUsageHandle(raw); err != nil {
		t.Fatal(err)
	}
	if trials.State(41, now) != TrialNone {
		t.Fatal("actual successful usage handler did not clear trial")
	}
	if got := schedulerPickPublished(req, now.Add(time.Second)); got.AuthID != "success" {
		t.Fatalf("next ABI pick remained stale-excluded: %#v", got)
	}
	trials.TryBegin(42, now)
	_ = markResetProbeVerified(ResetProbeState{}, now)
	if trials.State(42, now) != TrialActive {
		t.Fatal("probe success cleared business-request trial evidence")
	}
}

func TestEvidenceConsumerMarksPendingAndQueueFullIsNonblocking(t *testing.T) {
	now := time.Now()
	previous := globalTrials
	globalTrials = NewTrialRegistry()
	t.Cleanup(func() { globalTrials = previous })
	globalTrials.TryBegin(21, now)
	consumeEvidenceIntent(EvidenceIntent{AuthID: "a", Instance: 21, BeganAt: now})
	globalTrials.Advance(21, now.Add(60*time.Second))
	if globalTrials.State(21, now) != TrialActive {
		t.Fatal("consumer did not retain pending trial at 60s")
	}
	trials := NewTrialRegistry()
	full := make(chan EvidenceIntent, 1)
	full <- EvidenceIntent{AuthID: "occupied"}
	PublishSchedulerSnapshot(&SchedulerSnapshot{HandleEnabled: true, Trials: trials, EvidenceIntents: full, Accounts: []AccountView{{ID: "b", Instance: 22, Cache: CacheUnknown}}, ActiveHighestTier: map[string]struct{}{"b": {}}})
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "b", Provider: "codex"}}}
	done := make(chan PickDecision, 1)
	go func() { done <- schedulerPickPublished(req, now) }()
	select {
	case got := <-done:
		if got.AuthID != "b" {
			t.Fatalf("pick=%#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("full evidence queue blocked pick")
	}
	trials.Advance(22, now.Add(60*time.Second))
	if trials.State(22, now) != TrialUnknown {
		t.Fatal("dropped intent trial not governed by timeout")
	}
}

func TestSchedulerPickObservesOneImmutablePublication(t *testing.T) {
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}}
	a := &SchedulerSnapshot{HandleEnabled: true, Accounts: []AccountView{{ID: "a", Instance: 31, Cache: CacheFresh}}, ActiveHighestTier: map[string]struct{}{"a": {}}}
	b := &SchedulerSnapshot{HandleEnabled: true, Accounts: []AccountView{{ID: "b", Instance: 32, Cache: CacheFresh}}, ActiveHighestTier: map[string]struct{}{"b": {}}}
	PublishSchedulerSnapshot(a)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			if i%2 == 0 {
				PublishSchedulerSnapshot(a)
			} else {
				PublishSchedulerSnapshot(b)
			}
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		got := schedulerPickPublished(req, time.Now())
		if got.AuthID != "a" && got.AuthID != "b" {
			t.Fatalf("mixed/torn snapshot decision=%#v", got)
		}
	}
	<-done
}
