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

func TestTrialUnknownExpiresIntoEligibility(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	r := NewTrialRegistry()
	if !r.TryBegin(9, now) {
		t.Fatal("begin")
	}
	r.Advance(9, now.Add(5*time.Minute))
	if got := r.State(9, now.Add(5*time.Minute)); got != TrialUnknown {
		t.Fatalf("state=%v", got)
	}
	if got := r.State(9, now.Add(6*time.Minute)); got != TrialNone {
		t.Fatalf("expired backoff state=%v", got)
	}
	if !r.TryBegin(9, now.Add(6*time.Minute)) {
		t.Fatal("backoff expiry did not permit CAS")
	}
}

func TestTrialBackoffSequence(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	want := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for retries, delay := range want {
		v := trialRecord{backoffs: retries}
		got := unknownTrial(v, now).nextRetry.Sub(now)
		if got != delay {
			t.Fatalf("retries=%d delay=%s want=%s", retries, got, delay)
		}
	}
}

func TestTrialThreeRetriesForceUnknown(t *testing.T) {
	now := time.Now()
	r := NewTrialRegistry()
	r.TryBegin(10, now)
	r.MarkEvidencePending(10, true)
	for i := 0; i < 2; i++ {
		r.ObserveRetry(10, now.Add(time.Duration(i+1)*time.Second))
		if r.State(10, now) != TrialActive {
			t.Fatalf("retry %d forced early", i+1)
		}
	}
	r.ObserveRetry(10, now.Add(3*time.Second))
	if r.State(10, now) != TrialUnknown {
		t.Fatal("third retry did not force unknown")
	}
}
