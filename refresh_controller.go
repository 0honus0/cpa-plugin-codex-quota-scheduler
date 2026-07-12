package main

import (
	"sync"
	"time"
)

type RefreshMode string

const (
	RefreshModeDormant RefreshMode = "dormant"
	RefreshModeActive  RefreshMode = "active"
)

type IntentSource string

const (
	IntentSourceNone          IntentSource = ""
	IntentSourceInitial       IntentSource = "scheduler_initial"
	IntentSourceStaleRecovery IntentSource = "scheduler_stale_recovery"
	IntentSourceInterval      IntentSource = "scheduler_interval"
)

// CacheSnapshot contains only the cache facts used to choose the unique normal
// refresh source. StaleAfter is deliberately informational: it never owns a
// controller deadline (INV-10/11).
type CacheSnapshot struct {
	Initial       bool
	StaleRecovery bool
	StaleAfter    time.Duration
	Accounts      []AccountState
}

func UniqueSchedulerSource(initial, staleRecovery, interval bool) IntentSource {
	switch {
	case initial:
		return IntentSourceInitial
	case staleRecovery:
		return IntentSourceStaleRecovery
	case interval:
		return IntentSourceInterval
	default:
		return IntentSourceNone
	}
}

type RefreshController struct {
	mu           sync.Mutex
	interval     time.Duration
	activeWindow time.Duration
	mode         RefreshMode
	lastActivity time.Time
	nextInterval time.Time
}

func NewRefreshController(interval, activeWindow time.Duration) *RefreshController {
	defaults := DefaultConfig()
	if interval <= 0 {
		interval = defaults.QuotaRefreshInterval
	}
	if activeWindow <= 0 {
		activeWindow = defaults.RefreshActiveWindow
	}
	return &RefreshController{interval: interval, activeWindow: activeWindow, mode: RefreshModeDormant}
}

func (c *RefreshController) OnPick(now time.Time, snapshot CacheSnapshot) []Intent {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mode = RefreshModeActive
	c.lastActivity = now
	if c.nextInterval.IsZero() {
		c.nextInterval = now.Add(c.interval)
	}
	source := UniqueSchedulerSource(snapshot.Initial, snapshot.StaleRecovery, false)
	if source == IntentSourceNone {
		return nil
	}
	return []Intent{{Source: string(source), Class: OperationLegacyRefresh}}
}

func (c *RefreshController) OnDeadline(now time.Time) []Intent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode != RefreshModeActive || c.lastActivity.IsZero() || !now.Before(c.lastActivity.Add(c.activeWindow)) {
		c.mode = RefreshModeDormant
		c.nextInterval = time.Time{}
		return nil
	}
	if c.nextInterval.IsZero() || now.Before(c.nextInterval) {
		return nil
	}
	for !c.nextInterval.After(now) {
		c.nextInterval = c.nextInterval.Add(c.interval)
	}
	return []Intent{{Source: string(IntentSourceInterval), Class: OperationLegacyRefresh}}
}

func (c *RefreshController) Mode(now time.Time) RefreshMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode == RefreshModeActive && !c.lastActivity.IsZero() && !now.Before(c.lastActivity.Add(c.activeWindow)) {
		c.mode = RefreshModeDormant
		c.nextInterval = time.Time{}
	}
	return c.mode
}

func (c *RefreshController) NextDeadline(now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode != RefreshModeActive || c.lastActivity.IsZero() {
		return time.Time{}
	}
	activeDeadline := c.lastActivity.Add(c.activeWindow)
	if !now.Before(activeDeadline) {
		c.mode = RefreshModeDormant
		c.nextInterval = time.Time{}
		return time.Time{}
	}
	if c.nextInterval.IsZero() || activeDeadline.Before(c.nextInterval) {
		return activeDeadline
	}
	return c.nextInterval
}
