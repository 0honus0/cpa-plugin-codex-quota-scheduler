package main

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

const (
	rosterActiveTTL     = 5 * time.Minute
	rosterDegradedLimit = 30 * time.Minute
	provisionalMaxAge   = 4 * time.Hour
)

func provisionalAgeValid(now, confirmedAt time.Time) bool {
	if confirmedAt.IsZero() {
		return false
	}
	age := now.Sub(confirmedAt)
	return age >= 0 && age < provisionalMaxAge
}

type RosterHealth string

const (
	RosterWaiting    RosterHealth = "WaitingRoster"
	RosterHealthy    RosterHealth = "Healthy"
	RosterDegraded   RosterHealth = "Degraded"
	RosterFailClosed RosterHealth = "FailClosed"
)

// ActiveRoster is an immutable value. Instances and Entries are copied on
// publication and on every Snapshot return.
type ActiveRoster struct {
	Capability        HostCapability
	Confirmed         bool
	Provisional       bool
	HighestPriority   int
	Generation        uint64
	LifecycleRevision uint64
	Instances         []string
	Entries           []RosterEntry
	ConfirmedAt       time.Time
	LastSyncAt        time.Time
	DegradedSince     time.Time
	Health            RosterHealth
	BackgroundAllowed bool
}

type RosterControllerOptions struct {
	Host               HostAuthLister
	Now                func() time.Time
	Publish            func(context.Context, ActiveRoster) (ActiveRoster, error)
	Observe            func(ActiveRoster)
	Cancel             func([]string)
	Candidates         func() []string // deliberately never consulted for roster truth
	Provisional        *ActiveRoster
	ProbeOnProvisional bool
	VerifyProvisional  func(context.Context, ActiveRoster) bool
	CommitProvisional  func(func()) bool
}

type RosterController struct {
	mu                 sync.Mutex
	host               HostAuthLister
	now                func() time.Time
	publish            func(context.Context, ActiveRoster) (ActiveRoster, error)
	observe            func(ActiveRoster)
	cancel             func([]string)
	current            ActiveRoster
	inFlight           chan struct{}
	lastErr            error
	probeOnProvisional bool
	verifyProvisional  func(context.Context, ActiveRoster) bool
	commitProvisional  func(func()) bool
}

func NewRosterController(opts RosterControllerOptions) *RosterController {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	c := &RosterController{host: opts.Host, now: now, publish: opts.Publish, observe: opts.Observe, cancel: opts.Cancel, probeOnProvisional: opts.ProbeOnProvisional, verifyProvisional: opts.VerifyProvisional, commitProvisional: opts.CommitProvisional}
	c.current = ActiveRoster{Capability: CapabilityB, Health: RosterWaiting}
	if opts.Provisional != nil {
		p := cloneActiveRoster(*opts.Provisional)
		if !provisionalAgeValid(now(), p.ConfirmedAt) {
			p = ActiveRoster{Capability: CapabilityB, Health: RosterWaiting}
		} else {
			p.Capability, p.Confirmed, p.Provisional, p.Health = CapabilityB, false, true, RosterWaiting
			p.BackgroundAllowed = false
		}
		c.current = p
	}
	return c
}

func (c *RosterController) Snapshot() ActiveRoster {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneActiveRoster(c.current)
}

// EnforceLifecycle derives the effective lifecycle at a background request
// boundary. The transition is monotonic and published exactly once; it never
// changes the confirmed roster generation or the pick-visible roster.
func (c *RosterController) EnforceLifecycle(now time.Time) ActiveRoster {
	c.mu.Lock()
	active := cloneActiveRoster(c.current)
	transitioned := active.Confirmed &&
		active.Health == RosterDegraded &&
		!active.DegradedSince.IsZero() &&
		!now.Before(active.DegradedSince.Add(rosterDegradedLimit))
	if transitioned {
		active.Health = RosterFailClosed
		active.BackgroundAllowed = false
		active.LifecycleRevision++
		c.current = cloneActiveRoster(active)
	}
	c.mu.Unlock()
	if transitioned {
		c.notify(active)
	}
	return cloneActiveRoster(active)
}

func (c *RosterController) Startup(ctx context.Context) (ActiveRoster, error) {
	return c.sync(ctx, true)
}
func (c *RosterController) WakeForManagement(ctx context.Context) (ActiveRoster, error) {
	return c.sync(ctx, true)
}
func (c *RosterController) WakeForProbe(ctx context.Context) (ActiveRoster, error) {
	got, err := c.sync(ctx, false)
	resetProvisional := false
	if got.Provisional {
		c.mu.Lock()
		if c.current.Provisional && c.current.Generation == got.Generation && c.current.BackgroundAllowed {
			c.current.BackgroundAllowed = false
			c.current.LifecycleRevision++
			got = cloneActiveRoster(c.current)
			resetProvisional = true
		}
		c.mu.Unlock()
	}
	if got.Provisional && c.probeOnProvisional && c.verifyProvisional != nil && provisionalAgeValid(c.now(), got.ConfirmedAt) && c.verifyProvisional(ctx, cloneActiveRoster(got)) {
		committed := false
		commit := func() {
			c.mu.Lock()
			if c.current.Provisional && c.current.Generation == got.Generation && provisionalAgeValid(c.now(), got.ConfirmedAt) {
				c.current.BackgroundAllowed = true
				c.current.LifecycleRevision++
				got = cloneActiveRoster(c.current)
				committed = true
			}
			c.mu.Unlock()
		}
		if c.commitProvisional != nil {
			_ = c.commitProvisional(commit)
		} else {
			commit()
		}
		if committed {
			c.notify(got)
		}
	} else if resetProvisional {
		c.notify(got)
	}
	return got, err
}
func (c *RosterController) WakeForActivity(ctx context.Context) (ActiveRoster, error) {
	return c.sync(ctx, false)
}

func (c *RosterController) sync(ctx context.Context, force bool) (ActiveRoster, error) {
	for {
		c.mu.Lock()
		now := c.now()
		if force && c.lastErr == nil && !c.current.LastSyncAt.IsZero() && c.current.LastSyncAt.Equal(now) {
			out := cloneActiveRoster(c.current)
			err := c.lastErr
			c.mu.Unlock()
			return out, err
		}
		if !force && !c.current.LastSyncAt.IsZero() && now.Sub(c.current.LastSyncAt) < rosterActiveTTL {
			out := cloneActiveRoster(c.current)
			c.mu.Unlock()
			return out, nil
		}
		if wait := c.inFlight; wait != nil {
			c.mu.Unlock()
			select {
			case <-wait:
				return c.Snapshot(), c.lastSyncError()
			case <-ctx.Done():
				return c.Snapshot(), ctx.Err()
			}
		}
		c.inFlight = make(chan struct{})
		c.mu.Unlock()
		entries, err := c.list(ctx)
		return c.finishSync(ctx, entries, err, now)
	}
}

func (c *RosterController) list(ctx context.Context) ([]RosterEntry, error) {
	if c.host == nil {
		return nil, errors.New("authoritative host roster unavailable")
	}
	return c.host.ListHostAuths(ctx)
}

// OnSyncResult is the controller's only publication path, including injected
// host results used by startup adapters.
func (c *RosterController) OnSyncResult(ctx context.Context, entries []RosterEntry, err error) (ActiveRoster, error) {
	return c.finishSync(ctx, entries, err, c.now())
}

func (c *RosterController) finishSync(ctx context.Context, entries []RosterEntry, syncErr error, now time.Time) (ActiveRoster, error) {
	c.mu.Lock()
	if c.inFlight == nil {
		c.inFlight = make(chan struct{})
	}
	done := c.inFlight
	old := cloneActiveRoster(c.current)
	if syncErr == nil {
		priority, ids, ok := HighestCodexTier(entries)
		if !ok {
			syncErr = errors.New("authoritative host roster has no confirmed codex tier")
		} else {
			filtered := filterRosterEntries(entries, ids)
			generation := old.Generation
			if !sameRoster(old, priority, filtered, ids) {
				generation++
			}
			next := ActiveRoster{Capability: CapabilityA, Confirmed: true, HighestPriority: priority, Generation: generation, LifecycleRevision: old.LifecycleRevision + 1, Instances: ids, Entries: filtered, ConfirmedAt: now, LastSyncAt: now, Health: RosterHealthy, BackgroundAllowed: true}
			c.mu.Unlock()
			if c.publish != nil {
				var committed ActiveRoster
				committed, syncErr = c.publish(ctx, cloneActiveRoster(next))
				if syncErr == nil && committed.Generation > next.Generation {
					next.Generation = committed.Generation
				}
			}
			c.mu.Lock()
			if syncErr == nil {
				if next.LifecycleRevision <= c.current.LifecycleRevision {
					next.LifecycleRevision = c.current.LifecycleRevision + 1
				}
				c.current = next
				removed := removedRosterIDs(old.Instances, next.Instances)
				if len(removed) > 0 && c.cancel != nil {
					c.cancel(removed)
				}
			}
		}
	}
	if syncErr != nil {
		c.current = degradedRoster(old, now)
		c.current.LifecycleRevision = old.LifecycleRevision + 1
	}
	c.lastErr = syncErr
	c.inFlight = nil
	close(done)
	out := cloneActiveRoster(c.current)
	c.mu.Unlock()
	c.notify(out)
	return out, syncErr
}

func degradedRoster(old ActiveRoster, now time.Time) ActiveRoster {
	old.LastSyncAt = now
	old.BackgroundAllowed = false
	if old.Provisional {
		old.Capability, old.Confirmed, old.Health = CapabilityB, false, RosterWaiting
		return old
	}
	if old.ConfirmedAt.IsZero() {
		old.Capability, old.Confirmed, old.Health = CapabilityB, false, RosterWaiting
		return old
	}
	if old.DegradedSince.IsZero() {
		old.DegradedSince = now
	}
	if now.Sub(old.DegradedSince) < rosterDegradedLimit {
		old.Health, old.BackgroundAllowed = RosterDegraded, true
	} else {
		old.Health = RosterFailClosed
	}
	return old
}

func (c *RosterController) notify(active ActiveRoster) {
	if c.observe != nil {
		c.observe(cloneActiveRoster(active))
	}
}

func sameRoster(old ActiveRoster, priority int, entries []RosterEntry, ids []string) bool {
	if !old.Confirmed || old.HighestPriority != priority || len(old.Instances) != len(ids) || len(old.Entries) != len(entries) {
		return false
	}
	for i := range ids {
		if old.Instances[i] != ids[i] {
			return false
		}
	}
	for i := range entries {
		a, b := old.Entries[i], entries[i]
		if a.ID != b.ID || a.AuthIndex != b.AuthIndex || a.Provider != b.Provider || a.Priority == nil || b.Priority == nil || *a.Priority != *b.Priority {
			return false
		}
	}
	return true
}

func (c *RosterController) lastSyncError() error { c.mu.Lock(); defer c.mu.Unlock(); return c.lastErr }

func cloneActiveRoster(in ActiveRoster) ActiveRoster {
	in.Instances = append([]string(nil), in.Instances...)
	in.Entries = append([]RosterEntry(nil), in.Entries...)
	return in
}

func filterRosterEntries(entries []RosterEntry, ids []string) []RosterEntry {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	out := make([]RosterEntry, 0, len(ids))
	for _, entry := range entries {
		if _, ok := set[entry.ID]; ok {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func removedRosterIDs(old, next []string) []string {
	keep := make(map[string]struct{}, len(next))
	for _, id := range next {
		keep[id] = struct{}{}
	}
	var out []string
	for _, id := range old {
		if _, ok := keep[id]; !ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
