package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSuiteCoordinator(t *testing.T) {
	//inv:INV-04 positive
	//inv:INV-04 negative
	//inv:INV-05 positive
	//inv:INV-05 negative
	//inv:INV-09 positive
	//inv:INV-09 negative
	//inv:INV-24 positive
	//inv:INV-24 negative
	//inv:INV-25 positive
	//inv:INV-25 negative
	//inv:INV-26 positive
	//inv:INV-26 negative
	//inv:INV-42 positive
	//inv:INV-42 negative
	//inv:INV-46 positive
	//inv:INV-46 negative
	t.Run("deduplicates same instance class and generation", func(t *testing.T) {
		started := make(chan struct{}, 2)
		release := make(chan struct{})
		c := NewCoordinator(CoordinatorOptions{MaxHTTPSlots: 1, LeaseDuration: 2 * time.Minute, Execute: func(ctx context.Context, intent Intent, held *HeldLease) OperationResult {
			started <- struct{}{}
			<-release
			return OperationResult{Token: intent.Token}
		}})
		defer c.Close()
		intent := Intent{Instance: 7, Generation: 3, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 7, Tier: 3}}
		one := c.Submit(intent)
		two := c.Submit(intent)
		<-started
		select {
		case <-started:
			t.Fatal("deduplicated operation executed twice")
		default:
		}
		close(release)
		if one.Await(context.Background()).Err != nil || two.Await(context.Background()).Err != nil {
			t.Fatal("deduplicated futures failed")
		}
	})

	t.Run("discards result after lease expiry", func(t *testing.T) {
		now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
		clock := newCoordinatorTestClock(now)
		started := make(chan struct{})
		release := make(chan struct{})
		c := NewCoordinator(CoordinatorOptions{MaxHTTPSlots: 1, LeaseDuration: 2 * time.Minute, Now: clock.Now, AfterFunc: clock.AfterFunc, Execute: func(ctx context.Context, intent Intent, held *HeldLease) OperationResult {
			close(started)
			<-release
			return OperationResult{Token: intent.Token}
		}})
		defer c.Close()
		future := c.Submit(Intent{Instance: 9, Generation: 1, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 9, Tier: 1}})
		<-started
		clock.Advance(2 * time.Minute)
		close(release)
		if got := future.Await(context.Background()); got.Disposition != ResultDiscardedStale {
			t.Fatalf("disposition = %q, want stale", got.Disposition)
		}
	})

	t.Run("drain cancels joins and inherits uncertain probe", func(t *testing.T) {
		started := make(chan struct{})
		cancelled := make(chan struct{})
		release := make(chan struct{})
		inherited := make(chan time.Time, 1)
		c := NewCoordinator(CoordinatorOptions{MaxHTTPSlots: 1, Execute: func(ctx context.Context, intent Intent, held *HeldLease) OperationResult {
			held.MarkProbeSent(time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC))
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-release
			return OperationResult{Token: intent.Token}
		}, InheritSentUnknown: func(_ Intent, until time.Time) { inherited <- until }})
		defer c.Close()
		future := c.Submit(Intent{Instance: 11, Generation: 1, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 11, Tier: 1, Fence: 1}})
		<-started
		drained := make(chan DrainReport, 1)
		go func() { drained <- c.DrainLegacy(context.Background()) }()
		<-cancelled
		close(release)
		report := <-drained
		if report.Cancelled != 1 || report.Invalidated != 1 || report.SentUnknown != 1 {
			t.Fatalf("drain report = %+v", report)
		}
		if got := future.Await(context.Background()); got.Disposition != ResultDiscardedStale {
			t.Fatalf("drained result = %q", got.Disposition)
		}
		select {
		case <-inherited:
		default:
			t.Fatal("SentUnknown was not inherited")
		}
	})
}

func TestMockGroupECoordinatorInterleavings(t *testing.T) { TestSuiteCoordinator(t) }

type coordinatorTestClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []coordinatorTestTimer
}
type coordinatorTestTimer struct {
	at time.Time
	fn func()
}

func newCoordinatorTestClock(now time.Time) *coordinatorTestClock {
	return &coordinatorTestClock{now: now}
}
func (c *coordinatorTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *coordinatorTestClock) AfterFunc(d time.Duration, fn func()) {
	c.mu.Lock()
	c.timers = append(c.timers, coordinatorTestTimer{c.now.Add(d), fn})
	c.mu.Unlock()
}
func (c *coordinatorTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var run []func()
	keep := c.timers[:0]
	for _, tm := range c.timers {
		if !tm.at.After(c.now) {
			run = append(run, tm.fn)
		} else {
			keep = append(keep, tm)
		}
	}
	c.timers = keep
	c.mu.Unlock()
	for _, fn := range run {
		fn()
	}
}
