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
	Instances         []string
	Entries           []RosterEntry
	ConfirmedAt       time.Time
	LastSyncAt        time.Time
	Health            RosterHealth
	BackgroundAllowed bool
}

type RosterControllerOptions struct {
	Host               HostAuthLister
	Now                func() time.Time
	Publish            func(context.Context, ActiveRoster) error
	Cancel             func([]string)
	Candidates         func() []string // deliberately never consulted for roster truth
	Provisional        *ActiveRoster
	ProbeOnProvisional bool
	VerifyProvisional  func(context.Context, ActiveRoster) bool
}

type RosterController struct {
	mu                 sync.Mutex
	host               HostAuthLister
	now                func() time.Time
	publish            func(context.Context, ActiveRoster) error
	cancel             func([]string)
	current            ActiveRoster
	inFlight           chan struct{}
	lastErr            error
	probeOnProvisional bool
	verifyProvisional  func(context.Context, ActiveRoster) bool
}

func NewRosterController(opts RosterControllerOptions) *RosterController {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	c := &RosterController{host: opts.Host, now: now, publish: opts.Publish, cancel: opts.Cancel, probeOnProvisional: opts.ProbeOnProvisional, verifyProvisional: opts.VerifyProvisional}
	c.current = ActiveRoster{Capability: CapabilityB, Health: RosterWaiting}
	if opts.Provisional != nil {
		p := cloneActiveRoster(*opts.Provisional)
		if p.ConfirmedAt.IsZero() || now().Sub(p.ConfirmedAt) >= provisionalMaxAge {
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

func (c *RosterController) Startup(ctx context.Context) (ActiveRoster, error) {
	return c.sync(ctx, true)
}
func (c *RosterController) WakeForManagement(ctx context.Context) (ActiveRoster, error) {
	return c.sync(ctx, true)
}
func (c *RosterController) WakeForProbe(ctx context.Context) (ActiveRoster, error) {
	got, err := c.sync(ctx, false)
	if got.Provisional && c.probeOnProvisional && c.verifyProvisional != nil && c.now().Sub(got.ConfirmedAt) < provisionalMaxAge && c.verifyProvisional(ctx, cloneActiveRoster(got)) {
		c.mu.Lock()
		if c.current.Provisional && c.current.Generation == got.Generation {
			c.current.BackgroundAllowed = true
			got = cloneActiveRoster(c.current)
		}
		c.mu.Unlock()
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
			next := ActiveRoster{Capability: CapabilityA, Confirmed: true, HighestPriority: priority, Generation: old.Generation + 1, Instances: ids, Entries: filtered, ConfirmedAt: now, LastSyncAt: now, Health: RosterHealthy, BackgroundAllowed: true}
			c.mu.Unlock()
			if c.publish != nil {
				syncErr = c.publish(ctx, cloneActiveRoster(next))
			}
			c.mu.Lock()
			if syncErr == nil {
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
	}
	c.lastErr = syncErr
	c.inFlight = nil
	close(done)
	out := cloneActiveRoster(c.current)
	c.mu.Unlock()
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
	if now.Sub(old.ConfirmedAt) < rosterDegradedLimit {
		old.Health, old.BackgroundAllowed = RosterDegraded, true
	} else {
		old.Health = RosterFailClosed
	}
	return old
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
