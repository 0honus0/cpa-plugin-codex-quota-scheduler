package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type pickActivityIntent struct {
	Request pluginapi.SchedulerPickRequest
	Version uint64
	Now     time.Time
}

type pickActivityPump struct {
	latest    atomic.Pointer[pickActivityIntent]
	wake      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	stopped   atomic.Bool
	stopOnce  sync.Once
	roster    *RosterController
	refresher *QuotaRefresher
}

func newPickActivityPump(roster *RosterController, refresher *QuotaRefresher) *pickActivityPump {
	ctx, cancel := context.WithCancel(context.Background())
	p := &pickActivityPump{
		wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}),
		roster: roster, refresher: refresher,
	}
	go p.run()
	return p
}

func (p *pickActivityPump) enqueue(req pluginapi.SchedulerPickRequest, version uint64, now time.Time) {
	if p == nil || p.stopped.Load() {
		return
	}
	copy := req
	copy.Candidates = append([]pluginapi.SchedulerAuthCandidate(nil), req.Candidates...)
	copy.Providers = append([]string(nil), req.Providers...)
	p.latest.Store(&pickActivityIntent{Request: copy, Version: version, Now: now})
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *pickActivityPump) run() {
	defer close(p.done)
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.wake:
			for {
				intent := p.latest.Swap(nil)
				if intent == nil {
					break
				}
				if p.roster != nil {
					_, _ = p.roster.WakeForActivity(p.ctx)
				}
				if p.ctx.Err() != nil {
					return
				}
				if p.refresher != nil {
					p.refresher.OnSchedulerPick(intent.Request, intent.Version, intent.Now)
				}
			}
		}
	}
}

func (p *pickActivityPump) stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		p.stopped.Store(true)
		p.cancel()
	})
}

var globalPickActivityPump atomic.Pointer[pickActivityPump]

func replaceGlobalPickActivityPump(roster *RosterController, refresher *QuotaRefresher) {
	next := newPickActivityPump(roster, refresher)
	previous := globalPickActivityPump.Swap(next)
	if previous != nil {
		previous.stop()
	}
}

func stopGlobalPickActivityPump() {
	if p := globalPickActivityPump.Swap(nil); p != nil {
		p.stop()
	}
}
