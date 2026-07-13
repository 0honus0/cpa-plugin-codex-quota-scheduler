package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSuiteCoordinator(t *testing.T) {
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
		}, InheritSentUnknown: func(_ Intent, until time.Time) error { inherited <- until; return nil }})
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

func TestRosterRemovalCancelsInFlightAndFencesWriteback(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var applied int
	c := NewCoordinator(CoordinatorOptions{
		Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
			if intent.Class == OperationLegacyRefresh {
				close(started)
				<-release // deliberately return late after cancellation
			}
			return OperationResult{Token: intent.Token}
		},
		Apply: func(Intent, OperationResult) error {
			applied++
			return nil
		},
	})
	defer c.Close()

	active := c.Submit(Intent{Instance: 41, Generation: 7, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 41, Tier: 7}})
	<-started
	queued := c.Submit(Intent{Instance: 41, Generation: 7, Class: OperationQuotaRead, Source: SourceSchedulerInterval, Token: ExecutionToken{Instance: 41, Tier: 7}})

	c.CancelInstances([]AuthInstanceID{41})
	if got := queued.Await(context.Background()); got.Disposition != ResultCancelled {
		t.Fatalf("queued disposition=%q, want cancelled", got.Disposition)
	}
	if got := active.Await(context.Background()); got.Disposition != ResultCancelled {
		t.Fatalf("active disposition=%q, want cancelled", got.Disposition)
	}
	close(release)
	time.Sleep(10 * time.Millisecond)
	if applied != 0 {
		t.Fatalf("late cancelled result applied %d writebacks", applied)
	}

	stale := c.Submit(Intent{Instance: 41, Generation: 7, Class: OperationQuotaRead, Source: SourceSchedulerInterval, Token: ExecutionToken{Instance: 41, Tier: 7}}).Await(context.Background())
	if stale.Disposition != ResultCancelled {
		t.Fatalf("post-removal stale submission disposition=%q", stale.Disposition)
	}
}

func TestCancelInstancesDoesNotDeadlockTypedSubmission(t *testing.T) {
	release := make(chan struct{})
	c := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		<-release
		return OperationResult{Token: intent.Token}
	}})
	defer c.Close()
	active := c.Submit(Intent{Instance: 52, Generation: 3, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 52, Tier: 3}})

	c.typedMu.Lock()
	cancelDone := make(chan struct{})
	go func() {
		c.CancelInstances([]AuthInstanceID{52})
		close(cancelDone)
	}()
	if got := active.Await(context.Background()); got.Disposition != ResultCancelled {
		t.Fatalf("active disposition=%q", got.Disposition)
	}
	submitDone := make(chan struct{})
	go func() {
		_ = c.Submit(Intent{Instance: 52, Generation: 3, Class: OperationQuotaRead, Source: SourceSchedulerInterval, Token: ExecutionToken{Instance: 52, Tier: 3}})
		close(submitDone)
	}()
	select {
	case <-submitDone:
	case <-time.After(time.Second):
		c.typedMu.Unlock()
		<-cancelDone
		close(release)
		t.Fatal("CancelInstances held lifecycle authorization while waiting for typed state")
	}
	c.typedMu.Unlock()
	select {
	case <-cancelDone:
	case <-time.After(time.Second):
		t.Fatal("CancelInstances did not finish")
	}
	close(release)
}

func TestCancelledInstanceRejectsHigherGenerationUntilAuthoritativeActivation(t *testing.T) {
	var starts int
	c := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		starts++
		return OperationResult{Token: intent.Token}
	}})
	defer c.Close()
	initial := c.Submit(Intent{Instance: 61, Generation: 3, Class: OperationQuotaRead, Source: SourceSchedulerInterval, Token: ExecutionToken{Instance: 61, Tier: 3}}).Await(context.Background())
	if initial.Disposition != ResultApplied {
		t.Fatalf("initial disposition=%q", initial.Disposition)
	}
	c.CancelInstances([]AuthInstanceID{61})
	bypass := c.Submit(Intent{Instance: 61, Generation: 99, Class: OperationQuotaRead, Source: SourceSchedulerInterval, Token: ExecutionToken{Instance: 61, Tier: 99}}).Await(context.Background())
	if bypass.Disposition != ResultCancelled || starts != 1 {
		t.Fatalf("higher-generation bypass disposition=%q starts=%d", bypass.Disposition, starts)
	}
	c.activateInstances(map[AuthInstanceID]TierGeneration{61: 99})
	reactivated := c.Submit(Intent{Instance: 61, Generation: 99, Class: OperationQuotaRead, Source: SourceSchedulerInterval, Token: ExecutionToken{Instance: 61, Tier: 99}}).Await(context.Background())
	if reactivated.Disposition != ResultApplied || starts != 2 {
		t.Fatalf("reactivated disposition=%q starts=%d", reactivated.Disposition, starts)
	}
}

func TestMockGroupECoordinatorInterleavings(t *testing.T) { TestSuiteCoordinator(t) }

func TestCoordinatorNonCooperativeExpiryReleasesLease(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	clock := newCoordinatorTestClock(now)
	firstStarted, releaseFirst, secondStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := NewCoordinator(CoordinatorOptions{LeaseDuration: 2 * time.Minute, Now: clock.Now, AfterFunc: clock.AfterFunc, Execute: func(_ context.Context, intent Intent, _ *HeldLease) OperationResult {
		if intent.Token.Fence == 1 {
			close(firstStarted)
			<-releaseFirst
		} else {
			close(secondStarted)
		}
		return OperationResult{Token: intent.Token}
	}})
	defer c.Close()
	one := c.Submit(Intent{Instance: 1, Generation: 1, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 1, Tier: 1, Fence: 1}})
	<-firstStarted
	two := c.Submit(Intent{Instance: 1, Generation: 2, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, StartedAfter: 1, Token: ExecutionToken{Instance: 1, Tier: 2, Fence: 2}})
	clock.Advance(2 * time.Minute)
	if got := one.Await(context.Background()); got.Disposition != ResultDiscardedStale {
		t.Fatalf("expired result = %q", got.Disposition)
	}
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("queued work did not start after expiry")
	}
	close(releaseFirst)
	if got := two.Await(context.Background()); got.Disposition != ResultApplied {
		t.Fatalf("second result = %q", got.Disposition)
	}
}

func TestCoordinatorRejectsEveryStaleBindingComponentBeforeApply(t *testing.T) {
	fp := NewCredentialFingerprint("subject", "refresh", "meta")
	current := BindingVersion{Instance: 1, Admission: 2, Tier: 3, Login: 4, Fingerprint: fp}
	cases := []struct {
		name  string
		write WritebackVersion
	}{
		{"instance", WritebackVersion{Token: ExecutionToken{Instance: 9, Admission: 2, Tier: 3}, Login: 4, Fingerprint: fp}},
		{"admission", WritebackVersion{Token: ExecutionToken{Instance: 1, Admission: 9, Tier: 3}, Login: 4, Fingerprint: fp}},
		{"generation", WritebackVersion{Token: ExecutionToken{Instance: 1, Admission: 2, Tier: 9}, Login: 4, Fingerprint: fp}},
		{"login", WritebackVersion{Token: ExecutionToken{Instance: 1, Admission: 2, Tier: 3}, Login: 9, Fingerprint: fp}},
		{"fingerprint", WritebackVersion{Token: ExecutionToken{Instance: 1, Admission: 2, Tier: 3}, Login: 4, Fingerprint: NewCredentialFingerprint("other", "refresh", "meta")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied := 0
			c := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, i Intent, _ *HeldLease) OperationResult {
				return OperationResult{Token: tc.write.Token, Login: tc.write.Login, Fingerprint: tc.write.Fingerprint, Journal: &LegacyEffectJournal{}}
			}, Validate: func(_ Intent, r OperationResult) bool {
				return ValidateWriteback(current, WritebackVersion{Token: r.Token, Login: r.Login, Fingerprint: r.Fingerprint})
			}, Apply: func(Intent, OperationResult) error { applied++; return nil }})
			defer c.Close()
			got := c.Submit(Intent{Instance: 1, Generation: 3, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: tc.write.Token}).Await(context.Background())
			if got.Disposition != ResultDiscardedStale || applied != 0 {
				t.Fatalf("result=%q applied=%d", got.Disposition, applied)
			}
		})
	}
}

func TestCoordinatorCloseSubmitSnapshotRaceDoesNotHang(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, i Intent, _ *HeldLease) OperationResult {
		return OperationResult{Token: i.Token}
	}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			_ = c.PublishSnapshot()
			_ = c.Submit(Intent{Instance: AuthInstanceID(i + 1), Generation: 1, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: AuthInstanceID(i + 1), Tier: 1}})
		}
	}()
	c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("submit/snapshot hung racing close")
	}
}

func TestCoordinatorIdleDrainCompletesZeroJobs(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{})
	defer c.Close()
	if got := c.DrainLegacy(context.Background()); got.Completed != 0 {
		t.Fatalf("idle completed=%d", got.Completed)
	}
}

func TestCoordinatorLogCallbackWorkerRetainsLeaseAndRevalidates(t *testing.T) {
	var mu sync.Mutex
	valid := true
	applied := 0
	callbackStarted, release := make(chan struct{}), make(chan struct{})
	c := NewCoordinator(CoordinatorOptions{Execute: func(_ context.Context, i Intent, _ *HeldLease) OperationResult {
		return OperationResult{Token: i.Token, Journal: &LegacyEffectJournal{HostLogs: []BufferedHostLog{{Level: "warn", Message: "buffered"}}}}
	}, Validate: func(Intent, OperationResult) bool { mu.Lock(); defer mu.Unlock(); return valid }, DispatchLogs: func(ctx context.Context, _ Intent, _ OperationResult, held *HeldLease) error {
		return held.DoHTTP(ctx, func(context.Context) error { close(callbackStarted); <-release; return nil })
	}, Apply: func(Intent, OperationResult) error { applied++; return nil }})
	defer c.Close()
	f := c.Submit(Intent{Instance: 1, Generation: 1, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 1, Tier: 1, Fence: 1}})
	<-callbackStarted
	s := c.PublishSnapshot()
	if s.InFlight != 1 || len(s.LeasedInstances) != 1 {
		t.Fatalf("callback phase released lease: %+v", s)
	}
	mu.Lock()
	valid = false
	mu.Unlock()
	close(release)
	if got := f.Await(context.Background()); got.Disposition != ResultDiscardedStale || applied != 0 {
		t.Fatalf("result=%q applied=%d", got.Disposition, applied)
	}
}

func TestCoordinatorSentUnknownPersistenceFailureBlocksDrainHandoff(t *testing.T) {
	now := time.Unix(0, 0)
	clock := newCoordinatorTestClock(now)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := NewCoordinator(CoordinatorOptions{LeaseDuration: 2 * time.Minute, Now: clock.Now, AfterFunc: clock.AfterFunc, Execute: func(ctx context.Context, i Intent, h *HeldLease) OperationResult {
		h.MarkProbeSent(now.Add(10 * time.Minute))
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return OperationResult{Token: i.Token}
	}, InheritSentUnknown: func(Intent, time.Time) error { return errors.New("persist failed") }})
	defer c.Close()
	f := c.Submit(Intent{Instance: 1, Generation: 1, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 1, Tier: 1, Fence: 1}})
	<-started
	done := make(chan DrainReport, 1)
	go func() { done <- c.DrainLegacy(context.Background()) }()
	<-cancelled
	clock.Advance(2 * time.Minute)
	report := <-done
	if report.PersistenceFailures != 1 || !report.HandoffBlocked || report.SentUnknown != 0 {
		t.Fatalf("report=%+v", report)
	}
	if f.Await(context.Background()).Err == nil {
		t.Fatal("future did not surface persistence failure")
	}
	close(release)
}

func TestCoordinatorDrainCancelsBufferedLogWorker(t *testing.T) {
	occupied, releaseSlot := make(chan struct{}), make(chan struct{})
	callbackWaiting := make(chan struct{})
	applied := 0
	c := NewCoordinator(CoordinatorOptions{MaxHTTPSlots: 1, Execute: func(_ context.Context, i Intent, _ *HeldLease) OperationResult {
		return OperationResult{Token: i.Token, Journal: &LegacyEffectJournal{HostLogs: []BufferedHostLog{{Level: "warn"}}}}
	}, Validate: func(Intent, OperationResult) bool { return true }, DispatchLogs: func(ctx context.Context, _ Intent, _ OperationResult, h *HeldLease) error {
		close(callbackWaiting)
		return h.DoHTTP(ctx, func(context.Context) error { return nil })
	}, Apply: func(Intent, OperationResult) error { applied++; return nil }})
	go func() { _ = c.DoHostCallback(context.Background(), func() { close(occupied); <-releaseSlot }) }()
	<-occupied
	f := c.Submit(Intent{Instance: 1, Generation: 1, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 1, Tier: 1}})
	<-callbackWaiting
	report := c.DrainLegacy(context.Background())
	if report.Completed != 1 || applied != 0 {
		t.Fatalf("report=%+v applied=%d", report, applied)
	}
	if f.Await(context.Background()).Disposition != ResultDiscardedStale {
		t.Fatal("cancelled callback result applied")
	}
	close(releaseSlot)
	c.Close()
}

func TestDoHostCallbackCancellationAfterSlotWaitPreventsCall(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{MaxHTTPSlots: 1})
	defer c.Close()
	occupied, release := make(chan struct{}), make(chan struct{})
	go func() { _ = c.DoHostCallback(context.Background(), func() { close(occupied); <-release }) }()
	<-occupied
	ctx, cancel := context.WithCancel(context.Background())
	called := false
	done := make(chan error, 1)
	go func() { done <- c.DoHostCallback(ctx, func() { called = true }) }()
	cancel()
	close(release)
	if err := <-done; err == nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestQuotaRefresherStopDrainsAllQueuedWithinOneLeaseBound(t *testing.T) {
	clock := newCoordinatorTestClock(time.Unix(0, 0))
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := NewCoordinator(CoordinatorOptions{LeaseDuration: 2 * time.Minute, Now: clock.Now, AfterFunc: clock.AfterFunc, Execute: func(ctx context.Context, _ Intent, _ *HeldLease) OperationResult {
		select {
		case <-started:
		default:
			close(started)
		}
		<-ctx.Done()
		close(cancelled)
		<-release
		return OperationResult{}
	}})
	r := &QuotaRefresher{coordinator: c}
	futures := []Future[OperationResult]{}
	for i := 1; i <= 4; i++ {
		f := c.Submit(Intent{Instance: 1, Generation: TierGeneration(i), Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 1, Tier: TierGeneration(i), Fence: uint64(i)}})
		futures = append(futures, f)
		r.wg.Add(1)
		go func(f Future[OperationResult]) { defer r.wg.Done(); f.Await(context.Background()) }(f)
	}
	<-started
	done := make(chan struct{})
	go func() { r.Stop(); close(done) }()
	<-cancelled
	clock.Advance(2 * time.Minute)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop exceeded one virtual lease bound")
	}
	for _, f := range futures {
		select {
		case <-f.Done():
		default:
			t.Fatal("future not terminal after stop")
		}
	}
	close(release)
}

func TestCoordinatorDrainCancelsQueuedWithoutStarting(t *testing.T) {
	started := make(chan uint64, 2)
	cancelled := make(chan struct{})
	release := make(chan struct{})
	c := NewCoordinator(CoordinatorOptions{Execute: func(ctx context.Context, intent Intent, _ *HeldLease) OperationResult {
		started <- intent.Token.Fence
		if intent.Token.Fence == 1 {
			<-ctx.Done()
			close(cancelled)
			<-release
		}
		return OperationResult{Token: intent.Token}
	}})
	defer c.Close()
	one := c.Submit(Intent{Instance: 1, Generation: 1, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 1, Tier: 1, Fence: 1}})
	<-started
	two := c.Submit(Intent{Instance: 1, Generation: 2, Class: OperationLegacyRefresh, Source: LegacyRefreshSource, Token: ExecutionToken{Instance: 1, Tier: 2, Fence: 2}})
	done := make(chan DrainReport, 1)
	go func() { done <- c.DrainLegacy(context.Background()) }()
	<-cancelled
	close(release)
	report := <-done
	if report.Completed != 2 {
		t.Fatalf("completed jobs = %d, want 2", report.Completed)
	}
	select {
	case fence := <-started:
		t.Fatalf("queued fence %d started during drain", fence)
	default:
	}
	if two.Await(context.Background()).Disposition != ResultCancelled {
		t.Fatal("queued future was not cancelled")
	}
	_ = one
}

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
