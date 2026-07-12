package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTypedQuotaReadAllocatesAtActualStartAndBarrierJoin(t *testing.T) { //inv:INV-05,INV-26
	var seq atomic.Uint64
	started := make(chan Intent, 2)
	releaseLegacy := make(chan struct{})
	releaseRead := make(chan struct{})
	c := NewCoordinator(CoordinatorOptions{AllocateReadSeq: func() (uint64, error) { return seq.Add(1), nil }, Execute: func(ctx context.Context, i Intent, _ *HeldLease) OperationResult {
		if i.Class == OperationLegacyRefresh {
			<-releaseLegacy
		} else {
			started <- i
			<-releaseRead
		}
		return OperationResult{Token: i.Token, ReadStartSeq: i.ReadStartSeq}
	}})
	defer c.Close()
	legacy := c.Submit(Intent{Instance: 1, Generation: 1, Class: OperationLegacyRefresh, Source: LegacyEnvelopeSource, Token: ExecutionToken{Instance: 1, Fence: 1}})
	queued := c.SubmitTyped(Intent{Instance: 1, Generation: 1, Class: OperationQuotaRead, Source: SourceManualRefresh})
	if seq.Load() != 0 {
		t.Fatalf("queued read allocated seq=%d", seq.Load())
	}
	close(releaseLegacy)
	_ = legacy.Await(context.Background())
	actual := <-started
	if actual.ReadStartSeq == 0 {
		t.Fatal("actual read start seq missing")
	}
	joined := c.SubmitTyped(Intent{Instance: 1, Generation: 1, Class: OperationProbeVerify, Source: SourceProbeVerify, StartedAfter: actual.ReadStartSeq - 1, AttemptID: "a"})
	close(releaseRead)
	a, b := queued.Await(context.Background()), joined.Await(context.Background())
	if a.ReadStartSeq != b.ReadStartSeq || a.ReadStartSeq != actual.ReadStartSeq {
		t.Fatalf("queued=%d joined=%d actual=%d", a.ReadStartSeq, b.ReadStartSeq, actual.ReadStartSeq)
	}
}

func TestProbeVerifyDoesNotJoinAcrossAttempts(t *testing.T) {
	var seq atomic.Uint64
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	c := NewCoordinator(CoordinatorOptions{AllocateReadSeq: func() (uint64, error) { return seq.Add(1), nil }, Execute: func(_ context.Context, i Intent, _ *HeldLease) OperationResult {
		started <- struct{}{}
		<-release
		return OperationResult{Token: i.Token, ReadStartSeq: i.ReadStartSeq}
	}})
	defer c.Close()
	a := c.SubmitTyped(Intent{Instance: 1, Class: OperationProbeVerify, Source: SourceProbeVerify, AttemptID: "a"})
	<-started
	b := c.SubmitTyped(Intent{Instance: 1, Class: OperationProbeVerify, Source: SourceProbeVerify, AttemptID: "b"})
	close(release)
	_ = a.Await(context.Background())
	_ = b.Await(context.Background())
	if seq.Load() != 2 {
		t.Fatalf("read starts=%d", seq.Load())
	}
}

func TestCompletedFenceAndReadStartSeqRemainDistinct(t *testing.T) {
	raw, err := os.ReadFile("coordinator.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	if !strings.Contains(source, "completedFence :=") || !strings.Contains(source, "ReadStartSeq") || strings.Contains(source, "ReadStartSeq = completedFence") {
		t.Fatal("completedFence/read_start_seq semantics were aliased")
	}
}

func TestTypedProbeSendNeverJoinsSameAttempt(t *testing.T) { //inv:INV-06
	var mu sync.Mutex
	starts := 0
	release := make(chan struct{})
	c := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, i Intent, _ *HeldLease) OperationResult {
		mu.Lock()
		starts++
		mu.Unlock()
		<-release
		return OperationResult{Token: i.Token}
	}})
	defer c.Close()
	f1 := c.SubmitTyped(Intent{Instance: 1, Class: OperationProbeSend, Source: SourceProbeActivation, AttemptID: "same"})
	f2 := c.SubmitTyped(Intent{Instance: 1, Class: OperationProbeSend, Source: SourceProbeActivation, AttemptID: "same"})
	close(release)
	_ = f1.Await(context.Background())
	_ = f2.Await(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if starts != 2 {
		t.Fatalf("starts=%d", starts)
	}
}

func TestTypedPropagationWaitRetainsLeaseWithoutSlot(t *testing.T) { //inv:INV-07,INV-09
	wait := make(chan struct{})
	entered := make(chan struct{})
	other := make(chan struct{})
	c := NewCoordinator(CoordinatorOptions{MaxHTTPSlots: 1, PropagationWait: func(ctx context.Context, _ time.Duration) error {
		close(entered)
		select {
		case <-wait:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, Execute: func(ctx context.Context, i Intent, h *HeldLease) OperationResult {
		if i.Class == OperationProbeSend {
			if err := h.DoHTTP(ctx, func(context.Context) error { return nil }); err != nil {
				return OperationResult{Err: err}
			}
			if err := h.WaitPropagation(ctx, 3*time.Second); err != nil {
				return OperationResult{Err: err}
			}
		} else {
			_ = h.DoHTTP(ctx, func(context.Context) error { close(other); return nil })
		}
		return OperationResult{Token: i.Token}
	}})
	defer c.Close()
	f := c.SubmitTyped(Intent{Instance: 1, Class: OperationProbeSend, Source: SourceProbeActivation, AttemptID: "a"})
	<-entered
	s := c.PublishSnapshot()
	if s.HTTPSlotsInUse != 0 || len(s.LeasedInstances) != 1 {
		t.Fatalf("snapshot=%#v", s)
	}
	g := c.SubmitTyped(Intent{Instance: 2, Class: OperationQuotaRead, Source: SourceManualRefresh})
	<-other
	close(wait)
	_ = f.Await(context.Background())
	_ = g.Await(context.Background())
}

func TestTypedPropagationLeaseReclaimEntersSentUnknownWithoutResign(t *testing.T) { //inv:INV-27,INV-36
	var expire func()
	entered := make(chan struct{})
	var starts atomic.Int32
	unknown := make(chan struct{}, 1)
	c := NewCoordinator(CoordinatorOptions{AfterFunc: func(_ time.Duration, fn func()) { expire = fn }, PropagationWait: func(ctx context.Context, _ time.Duration) error { close(entered); <-ctx.Done(); return ctx.Err() }, InheritSentUnknown: func(Intent, time.Time) error { unknown <- struct{}{}; return nil }, Execute: func(ctx context.Context, i Intent, h *HeldLease) OperationResult {
		starts.Add(1)
		h.MarkProbeSent(time.Unix(100, 0))
		return OperationResult{Token: i.Token, Err: h.WaitPropagation(ctx, 3*time.Second)}
	}})
	defer c.Close()
	f := c.SubmitTyped(Intent{Instance: 1, Class: OperationProbeSend, Source: SourceProbeActivation, AttemptID: "a"})
	<-entered
	expire()
	result := f.Await(context.Background())
	if result.Err == nil {
		t.Fatal("lease reclaim returned success")
	}
	<-unknown
	if starts.Load() != 1 {
		t.Fatalf("starts=%d", starts.Load())
	}
}

func TestMockGroupETypedIntentInterleavings(t *testing.T) {
	t.Run("barrier", TestTypedQuotaReadAllocatesAtActualStartAndBarrierJoin)
	t.Run("wait", TestTypedPropagationWaitRetainsLeaseWithoutSlot)
}
func TestMockGroupALegacyEnvelopeWithTypedIntent(t *testing.T) {
	t.Run("legacy-typed", TestTypedQuotaReadAllocatesAtActualStartAndBarrierJoin)
}

func TestProbeSequenceHoldsInstanceLeaseThroughVerify(t *testing.T) {
	sequenceStarted := make(chan struct{})
	releaseVerify := make(chan struct{})
	writeStarted := make(chan struct{}, 1)
	c := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, i Intent, _ *HeldLease) OperationResult {
		if i.Class == OperationProbeSequence {
			close(sequenceStarted)
			<-releaseVerify
		} else {
			writeStarted <- struct{}{}
		}
		return OperationResult{Token: i.Token}
	}})
	defer c.Close()
	seq := c.SubmitTyped(Intent{Instance: 7, Class: OperationProbeSequence, Source: SourceProbeActivation, AttemptID: "a"})
	<-sequenceStarted
	queued := c.Submit(Intent{Instance: 7, Class: OperationLegacyRefresh, Source: LegacyEnvelopeSource})
	select {
	case <-writeStarted:
		t.Fatal("same-instance write ran before probe verify/release")
	default:
	}
	close(releaseVerify)
	_ = seq.Await(context.Background())
	_ = queued.Await(context.Background())
	select {
	case <-writeStarted:
	default:
		t.Fatal("queued write never ran after sequence release")
	}
}
