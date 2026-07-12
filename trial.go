package main

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	trialEvidencePendingGrace = 60 * time.Second
	trialEvidenceBudget       = 5 * time.Minute
	trialEvidenceMaxRetries   = 3
)

type EvidenceKind uint8

const (
	EvidenceUnknown EvidenceKind = iota
	EvidenceUsageFeedback
	EvidenceReliableQuotaWriteback
	EvidenceRequestSuccess
)

type Evidence struct {
	Kind EvidenceKind
	At   time.Time
}

type trialRecord struct {
	state     TrialState
	began     time.Time
	pending   bool
	retries   int
	backoffs  int
	nextRetry time.Time
}
type trialCell struct{ value atomic.Pointer[trialRecord] }
type TrialRegistry struct{ cells sync.Map }

func NewTrialRegistry() *TrialRegistry { return &TrialRegistry{} }
func (r *TrialRegistry) cell(id AuthInstanceID) *trialCell {
	v, _ := r.cells.LoadOrStore(id, &trialCell{})
	return v.(*trialCell)
}
func (r *TrialRegistry) TryBegin(id AuthInstanceID, now time.Time) bool {
	c := r.cell(id)
	for {
		old := c.value.Load()
		if old != nil && (old.state == TrialActive || now.Before(old.nextRetry)) {
			return false
		}
		backoffs := 0
		if old != nil {
			backoffs = old.backoffs
		}
		next := &trialRecord{state: TrialActive, began: now, backoffs: backoffs}
		if c.value.CompareAndSwap(old, next) {
			return true
		}
	}
}
func (r *TrialRegistry) ObserveEvidence(id AuthInstanceID, e Evidence) {
	if e.Kind == EvidenceUsageFeedback || e.Kind == EvidenceReliableQuotaWriteback || e.Kind == EvidenceRequestSuccess {
		r.cell(id).value.Store(nil)
	}
}
func (r *TrialRegistry) MarkEvidencePending(id AuthInstanceID, p bool) {
	r.update(id, func(v trialRecord) trialRecord { v.pending = p; return v })
}
func (r *TrialRegistry) ObserveRetry(id AuthInstanceID, now time.Time) {
	r.update(id, func(v trialRecord) trialRecord {
		v.retries++
		if v.retries >= trialEvidenceMaxRetries {
			return unknownTrial(v, now)
		}
		return v
	})
}
func (r *TrialRegistry) Advance(id AuthInstanceID, now time.Time) {
	r.update(id, func(v trialRecord) trialRecord {
		if v.state == TrialActive && (!now.Before(v.began.Add(trialEvidenceBudget)) || (!v.pending && !now.Before(v.began.Add(trialEvidencePendingGrace)))) {
			return unknownTrial(v, now)
		}
		return v
	})
}
func unknownTrial(v trialRecord, now time.Time) trialRecord {
	v.state = TrialUnknown
	delays := [...]time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}
	index := v.backoffs
	if index >= len(delays) {
		index = len(delays) - 1
	}
	delay := delays[index]
	v.backoffs++
	v.nextRetry = now.Add(delay)
	return v
}
func (r *TrialRegistry) State(id AuthInstanceID, now time.Time) TrialState {
	c := r.cell(id)
	v := c.value.Load()
	if v == nil {
		return TrialNone
	}
	if v.state == TrialUnknown && !now.Before(v.nextRetry) {
		return TrialNone
	}
	return v.state
}
func (r *TrialRegistry) NextRetryAt(id AuthInstanceID) time.Time {
	v := r.cell(id).value.Load()
	if v == nil {
		return time.Time{}
	}
	return v.nextRetry
}
func (r *TrialRegistry) update(id AuthInstanceID, f func(trialRecord) trialRecord) {
	c := r.cell(id)
	for {
		old := c.value.Load()
		if old == nil {
			return
		}
		next := f(*old)
		if c.value.CompareAndSwap(old, &next) {
			return
		}
	}
}
