package testsupport

import (
	"sort"
	"sync"
	"time"
)

type timer struct {
	at  time.Time
	seq uint64
	fn  func()
}

type Clock struct {
	mu     sync.Mutex
	now    time.Time
	seq    uint64
	timers []timer
}

func NewClock(now time.Time) *Clock { return &Clock{now: now} }
func (c *Clock) Now() time.Time     { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *Clock) AfterFunc(d time.Duration, fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	c.timers = append(c.timers, timer{at: c.now.Add(d), seq: c.seq, fn: fn})
}
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	sort.SliceStable(c.timers, func(i, j int) bool {
		if c.timers[i].at.Equal(c.timers[j].at) {
			return c.timers[i].seq < c.timers[j].seq
		}
		return c.timers[i].at.Before(c.timers[j].at)
	})
	var due []func()
	keep := c.timers[:0]
	for _, tm := range c.timers {
		if !tm.at.After(c.now) {
			due = append(due, tm.fn)
		} else {
			keep = append(keep, tm)
		}
	}
	c.timers = keep
	c.mu.Unlock()
	for _, fn := range due {
		fn()
	}
}
