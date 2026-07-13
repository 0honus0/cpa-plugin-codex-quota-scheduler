package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type rosterTestHost struct {
	mu      sync.Mutex
	calls   int
	entries []RosterEntry
	err     error
	gate    chan struct{}
}

func (h *rosterTestHost) ListHostAuths(ctx context.Context) ([]RosterEntry, error) {
	h.mu.Lock()
	h.calls++
	gate, entries, err := h.gate, append([]RosterEntry(nil), h.entries...), h.err
	h.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return entries, err
}

func (h *rosterTestHost) callCount() int { h.mu.Lock(); defer h.mu.Unlock(); return h.calls }

func (h *rosterTestHost) update(entries []RosterEntry, err error, gate chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries, h.err, h.gate = append([]RosterEntry(nil), entries...), err, gate
}
func (h *rosterTestHost) setGate(gate chan struct{}) { h.mu.Lock(); defer h.mu.Unlock(); h.gate = gate }
func (h *rosterTestHost) setError(err error)         { h.mu.Lock(); defer h.mu.Unlock(); h.err = err }

func TestSuiteRosterManagement(t *testing.T) {
	//inv:INV-31 positive
	//inv:INV-31 negative
	//inv:INV-34 positive
	//inv:INV-34 negative
	now := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	p9, p1 := 9, 1
	host := &rosterTestHost{entries: []RosterEntry{
		{ID: "low", Provider: "codex", Priority: &p1},
		{ID: "high-a", Provider: "codex", Priority: &p9},
		{ID: "high-b", Provider: "codex", Priority: &p9},
	}}
	clock := now
	var published []ActiveRoster
	c := NewRosterController(RosterControllerOptions{
		Host: host, Now: func() time.Time { return clock },
		Publish: func(_ context.Context, roster ActiveRoster) (ActiveRoster, error) {
			published = append(published, roster)
			return roster, nil
		},
	})

	t.Run("startup, ttl, idle, probe and management wakes", func(t *testing.T) {
		got, err := c.Startup(context.Background())
		if err != nil || !got.Confirmed || got.HighestPriority != 9 || len(got.Instances) != 2 {
			t.Fatalf("startup = %#v, %v", got, err)
		}
		clock = now.Add(rosterActiveTTL - time.Nanosecond)
		if _, err := c.WakeForActivity(context.Background()); err != nil {
			t.Fatal(err)
		}
		if host.callCount() != 1 {
			t.Fatalf("fresh activity calls=%d", host.callCount())
		}
		clock = now.Add(rosterActiveTTL)
		if _, err := c.WakeForProbe(context.Background()); err != nil {
			t.Fatal(err)
		}
		if host.callCount() != 2 {
			t.Fatalf("probe prewake calls=%d", host.callCount())
		}
		clock = clock.Add(time.Nanosecond)
		if _, err := c.WakeForManagement(context.Background()); err != nil {
			t.Fatal(err)
		}
		if host.callCount() != 3 {
			t.Fatalf("management calls=%d", host.callCount())
		}
		if len(published) != 3 {
			t.Fatalf("published=%d", len(published))
		}
	})

	t.Run("singleflight", func(t *testing.T) {
		gate := make(chan struct{})
		host.setGate(gate)
		clock = clock.Add(rosterActiveTTL)
		before := host.callCount()
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = c.WakeForManagement(context.Background()) }()
		}
		for host.callCount() == before {
			time.Sleep(time.Millisecond)
		}
		close(gate)
		host.setGate(nil)
		wg.Wait()
		if host.callCount() != before+1 {
			t.Fatalf("singleflight calls=%d want %d", host.callCount(), before+1)
		}
	})

	t.Run("degraded boundary, fail closed and recovery", func(t *testing.T) {
		host.setError(errors.New("offline"))
		generation := c.Snapshot().Generation
		firstFailure := clock.Add(time.Minute)
		clock = firstFailure
		got, err := c.WakeForManagement(context.Background())
		if err == nil || got.Health != RosterDegraded || !got.BackgroundAllowed {
			t.Fatalf("first failure=%#v err=%v", got, err)
		}
		clock = firstFailure.Add(rosterDegradedLimit)
		got, _ = c.WakeForManagement(context.Background())
		if got.Health != RosterFailClosed || got.BackgroundAllowed {
			t.Fatalf("at boundary=%#v", got)
		}
		clock = firstFailure.Add(rosterDegradedLimit + time.Nanosecond)
		got, _ = c.WakeForProbe(context.Background())
		if got.Health != RosterFailClosed || len(got.Instances) != 2 {
			t.Fatalf("after boundary=%#v", got)
		}
		host.setError(nil)
		got, err = c.WakeForManagement(context.Background())
		if err != nil || got.Health != RosterHealthy || !got.Confirmed || got.Generation != generation {
			t.Fatalf("recovery=%#v err=%v", got, err)
		}
	})
}

func TestStartupCapabilityBRecoversThroughRosterSynchronization(t *testing.T) {
	p := 4
	host := &rosterTestHost{err: errors.New("not ready")}
	now := time.Now()
	c := NewRosterController(RosterControllerOptions{Host: host, Now: func() time.Time { return now }})
	got, err := c.Startup(context.Background())
	if err == nil || got.Capability != CapabilityB || got.Health != RosterWaiting {
		t.Fatalf("startup=%#v err=%v", got, err)
	}
	host.update([]RosterEntry{{ID: "a", Provider: "codex", Priority: &p}}, nil, nil)
	got, err = c.WakeForManagement(context.Background())
	if err != nil || got.Capability != CapabilityA || !got.Confirmed {
		t.Fatalf("recovery=%#v err=%v", got, err)
	}
}

func TestRosterCandidatesHaveNoRosterSideEffects(t *testing.T) {
	host := &rosterTestHost{err: errors.New("unavailable")}
	c := NewRosterController(RosterControllerOptions{Host: host, Candidates: func() []string { return []string{"candidate-only"} }})
	got, _ := c.Startup(context.Background())
	if len(got.Instances) != 0 || got.Confirmed {
		t.Fatalf("candidate became roster: %#v", got)
	}
}

func TestCapabilityBProvisionalProbeRequiresRiskOptionAgeAndFingerprint(t *testing.T) {
	now := time.Now()
	base := ActiveRoster{Instances: []string{"a"}, ConfirmedAt: now.Add(-time.Hour)}
	verified := false
	c := NewRosterController(RosterControllerOptions{Host: &rosterTestHost{err: errors.New("B")}, Now: func() time.Time { return now }, Provisional: &base, ProbeOnProvisional: true, VerifyProvisional: func(context.Context, ActiveRoster) bool { return verified }})
	got, _ := c.WakeForProbe(context.Background())
	if !got.Provisional || got.BackgroundAllowed {
		t.Fatalf("unverified=%#v", got)
	}
	verified = true
	got, _ = c.WakeForProbe(context.Background())
	if !got.Provisional || !got.BackgroundAllowed {
		t.Fatalf("verified=%#v", got)
	}
	expired := base
	expired.ConfirmedAt = now.Add(-provisionalMaxAge)
	c = NewRosterController(RosterControllerOptions{Provisional: &expired, Now: func() time.Time { return now }, ProbeOnProvisional: true, VerifyProvisional: func(context.Context, ActiveRoster) bool { return true }})
	if got := c.Snapshot(); got.Provisional || len(got.Instances) != 0 {
		t.Fatalf("expired=%#v", got)
	}
}

func TestProductionProbePrewakesRosterController(t *testing.T) {
	p := 1
	host := &rosterTestHost{entries: []RosterEntry{{ID: "a", Provider: "codex", Priority: &p}}}
	controller := NewRosterController(RosterControllerOptions{Host: host})
	refresherMu.Lock()
	previous := globalRosterController
	globalRosterController = controller
	refresherMu.Unlock()
	t.Cleanup(func() { refresherMu.Lock(); globalRosterController = previous; refresherMu.Unlock() })
	_ = (&QuotaRefresher{}).RunProbeDueOnce(context.Background())
	if host.callCount() != 1 {
		t.Fatalf("probe roster calls=%d", host.callCount())
	}
}

func TestRosterDegradedStartsAtFirstFailure(t *testing.T) {
	p := 7
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	clock := now
	host := &rosterTestHost{entries: []RosterEntry{{ID: "a", Provider: "codex", Priority: &p}}}
	c := NewRosterController(RosterControllerOptions{Host: host, Now: func() time.Time { return clock }})
	if _, err := c.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.setError(errors.New("offline"))
	firstFailure := now.Add(20 * time.Minute)
	clock = firstFailure
	got, err := c.WakeForManagement(context.Background())
	if err == nil || got.Health != RosterDegraded || !got.DegradedSince.Equal(firstFailure) {
		t.Fatalf("first failure=%#v err=%v", got, err)
	}
	clock = firstFailure.Add(rosterDegradedLimit - time.Nanosecond)
	got, _ = c.WakeForManagement(context.Background())
	if got.Health != RosterDegraded || !got.DegradedSince.Equal(firstFailure) {
		t.Fatalf("before boundary=%#v", got)
	}
	clock = firstFailure.Add(rosterDegradedLimit)
	got, _ = c.WakeForManagement(context.Background())
	if got.Health != RosterFailClosed || !got.DegradedSince.Equal(firstFailure) {
		t.Fatalf("at boundary=%#v", got)
	}
}

func TestRosterGenerationChangesOnlyWithRoster(t *testing.T) {
	p := 7
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	clock := now
	host := &rosterTestHost{entries: []RosterEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Priority: &p}}}
	c := NewRosterController(RosterControllerOptions{Host: host, Now: func() time.Time { return clock }})
	first, err := c.Startup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	identical, err := c.WakeForManagement(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if identical.Generation != first.Generation {
		t.Fatalf("identical generation=%d want %d", identical.Generation, first.Generation)
	}
	host.update([]RosterEntry{{ID: "b", AuthIndex: "idx-b", Provider: "codex", Priority: &p}}, nil, nil)
	clock = clock.Add(time.Minute)
	changed, err := c.WakeForManagement(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed.Generation != first.Generation+1 {
		t.Fatalf("changed generation=%d want %d", changed.Generation, first.Generation+1)
	}
}

func TestRosterObserverSeesEveryTransition(t *testing.T) {
	p := 7
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	clock := now
	host := &rosterTestHost{entries: []RosterEntry{{ID: "a", Provider: "codex", Priority: &p}}}
	var observed []ActiveRoster
	c := NewRosterController(RosterControllerOptions{
		Host: host,
		Now:  func() time.Time { return clock },
		Observe: func(active ActiveRoster) {
			observed = append(observed, cloneActiveRoster(active))
		},
	})
	if _, err := c.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.setError(errors.New("offline"))
	clock = clock.Add(rosterActiveTTL)
	_, _ = c.WakeForManagement(context.Background())
	host.setError(nil)
	clock = clock.Add(time.Minute)
	if _, err := c.WakeForManagement(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 3 {
		t.Fatalf("observed=%d want 3: %#v", len(observed), observed)
	}
	if observed[0].Health != RosterHealthy || observed[1].Health != RosterDegraded || observed[2].Health != RosterHealthy {
		t.Fatalf("observer transitions=%#v", observed)
	}
}

func TestRosterObserverRejectsOutOfOrderTransition(t *testing.T) {
	p := 7
	clock := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	host := &rosterTestHost{entries: []RosterEntry{{ID: "a", Provider: "codex", Priority: &p}}}
	runtime := NewQuotaRefresher(&fakeHostClient{}, NewPluginState(DefaultConfig()), func() time.Time { return clock })
	degradedEntered := make(chan struct{})
	releaseDegraded := make(chan struct{})
	var once sync.Once
	c := NewRosterController(RosterControllerOptions{
		Host: host,
		Now:  func() time.Time { return clock },
		Observe: func(active ActiveRoster) {
			if active.Health == RosterDegraded {
				once.Do(func() { close(degradedEntered) })
				<-releaseDegraded
			}
			runtime.ObserveRosterLifecycle(active)
		},
	})
	if _, err := c.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.setError(errors.New("offline"))
	clock = clock.Add(time.Minute)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = c.WakeForManagement(context.Background())
	}()
	<-degradedEntered
	clock = clock.Add(rosterDegradedLimit)
	if got, _ := c.WakeForManagement(context.Background()); got.Health != RosterFailClosed {
		t.Fatalf("second transition=%#v", got)
	}
	close(releaseDegraded)
	<-firstDone
	if got := runtime.runtimeRoster(); got.Health != RosterFailClosed {
		t.Fatalf("stale observer overwrote fail-closed lifecycle: %#v", got)
	}
}

func TestRosterProvisionalObserverOnlyAfterGuardCommit(t *testing.T) {
	p := 7
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	base := ActiveRoster{Instances: []string{"a"}, Entries: []RosterEntry{{ID: "a", Provider: "codex", Priority: &p}}, ConfirmedAt: now.Add(-time.Hour), Generation: 3}
	host := &rosterTestHost{err: errors.New("offline")}
	var c *RosterController
	var observed []ActiveRoster
	c = NewRosterController(RosterControllerOptions{
		Host:               host,
		Now:                func() time.Time { return now },
		Provisional:        &base,
		ProbeOnProvisional: true,
		Observe: func(active ActiveRoster) {
			observed = append(observed, cloneActiveRoster(active))
		},
		VerifyProvisional: func(ctx context.Context, _ ActiveRoster) bool {
			_, _ = c.OnSyncResult(ctx, []RosterEntry{{ID: "a", Provider: "codex", Priority: &p}}, nil)
			return true
		},
	})
	_, _ = c.WakeForProbe(context.Background())
	if got := c.Snapshot(); got.Health != RosterHealthy || got.Provisional {
		t.Fatalf("current=%#v", got)
	}
	if len(observed) != 2 || observed[len(observed)-1].Health != RosterHealthy {
		t.Fatalf("lost guard published stale provisional transition: %#v", observed)
	}
}
