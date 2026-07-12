package main

import (
	"sync"
	"testing"
	"time"
)

// inv:INV-29,INV-41,INV-45
func TestTrialRegistryCASAndEvidence(t *testing.T) {
	now := time.Now()
	r := NewTrialRegistry()
	var wg sync.WaitGroup
	wins := make(chan bool, 32)
	for range 32 {
		wg.Add(1)
		go func() { defer wg.Done(); wins <- r.TryBegin(7, now) }()
	}
	wg.Wait()
	close(wins)
	n := 0
	for win := range wins {
		if win {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("wins=%d want 1", n)
	}
	r.ObserveEvidence(7, Evidence{Kind: EvidenceUsageFeedback, At: now.Add(time.Second)})
	if !r.TryBegin(7, now.Add(2*time.Second)) {
		t.Fatal("real evidence did not release trial")
	}
}

// inv:INV-41,INV-45
func TestTrialPendingAtSixtySecondsAndBudget(t *testing.T) {
	now := time.Now()
	r := NewTrialRegistry()
	r.TryBegin(8, now)
	r.MarkEvidencePending(8, true)
	r.Advance(8, now.Add(60*time.Second))
	if r.State(8, now.Add(60*time.Second)) != TrialActive {
		t.Fatal("pending trial released at 60s")
	}
	r.Advance(8, now.Add(5*time.Minute))
	if r.State(8, now.Add(5*time.Minute)) != TrialUnknown {
		t.Fatal("trial did not force unknown at 5m")
	}
	if got := r.NextRetryAt(8); !got.Equal(now.Add(6 * time.Minute)) {
		t.Fatalf("retry=%s", got)
	}
}
