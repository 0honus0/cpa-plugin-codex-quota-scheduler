package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type OperationClass string

const (
	OperationLegacyRefresh OperationClass = "legacy_refresh"
	LegacyRefreshSource                   = "legacy_refresh_txn"
	legacyLeaseDuration                   = 2 * time.Minute
)

type Intent struct {
	Instance     AuthInstanceID
	Generation   TierGeneration
	Class        OperationClass
	Source       string
	StartedAfter uint64
	Token        ExecutionToken
	Payload      any
}

type ResultDisposition string

const (
	ResultApplied        ResultDisposition = "applied"
	ResultDiscardedStale ResultDisposition = "discarded_stale"
	ResultCancelled      ResultDisposition = "cancelled"
)

type OperationResult struct {
	Value          any
	Err            error
	Token          ExecutionToken
	Admission      InstanceAdmissionEpoch
	Generation     TierGeneration
	Login          LoginEpoch
	Fingerprint    CredentialFingerprint
	ProbeSendPhase bool
	SuppressUntil  time.Time
	Disposition    ResultDisposition
}

type futureState struct {
	done   chan struct{}
	once   sync.Once
	mu     sync.RWMutex
	result OperationResult
}

type Future[T any] struct{ state *futureState }

func (f Future[T]) Await(ctx context.Context) T {
	var zero T
	if f.state == nil {
		return zero
	}
	select {
	case <-f.state.done:
		f.state.mu.RLock()
		defer f.state.mu.RUnlock()
		if v, ok := any(f.state.result).(T); ok {
			return v
		}
		return zero
	case <-ctx.Done():
		if v, ok := any(OperationResult{Err: ctx.Err(), Disposition: ResultCancelled}).(T); ok {
			return v
		}
		return zero
	}
}

func (f Future[T]) Done() <-chan struct{} {
	if f.state == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return f.state.done
}

func completeFuture(f *futureState, result OperationResult) {
	f.once.Do(func() { f.mu.Lock(); f.result = result; f.mu.Unlock(); close(f.done) })
}

type HeldLease struct {
	coordinator   *Coordinator
	intent        Intent
	probeMu       sync.Mutex
	probeSent     bool
	suppressUntil time.Time
}

func (h *HeldLease) DoHTTP(ctx context.Context, call func(context.Context) error) error {
	select {
	case h.coordinator.httpSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-h.coordinator.httpSlots }()
	return call(ctx)
}

func (h *HeldLease) MarkProbeSent(suppressUntil time.Time) {
	h.probeMu.Lock()
	h.probeSent = true
	if suppressUntil.After(h.suppressUntil) {
		h.suppressUntil = suppressUntil
	}
	h.probeMu.Unlock()
}

func (h *HeldLease) probeState() (bool, time.Time) {
	h.probeMu.Lock()
	defer h.probeMu.Unlock()
	return h.probeSent, h.suppressUntil
}

type CoordinatorOptions struct {
	MaxHTTPSlots       int
	LeaseDuration      time.Duration
	Now                func() time.Time
	AfterFunc          func(time.Duration, func())
	Execute            func(context.Context, Intent, *HeldLease) OperationResult
	Validate           func(Intent, OperationResult) bool
	InheritSentUnknown func(Intent, time.Time)
}

type SchedulerSnapshot struct {
	Accepting       bool
	InFlight        int
	Queued          int
	LeasedInstances []AuthInstanceID
	HTTPSlotsInUse  int
}

type DrainReport struct {
	Completed, Cancelled, Invalidated, SentUnknown int
	TimedOut                                       bool
}

type operationKey struct {
	instance   AuthInstanceID
	class      OperationClass
	generation TierGeneration
}
type coordinatorJob struct {
	intent  Intent
	future  *futureState
	ctx     context.Context
	cancel  context.CancelFunc
	held    *HeldLease
	expired bool
}
type submitCommand struct {
	intent Intent
	reply  chan Future[OperationResult]
}
type resultCommand struct {
	key    operationKey
	job    *coordinatorJob
	result OperationResult
}
type expireCommand struct {
	key operationKey
	job *coordinatorJob
}
type snapshotCommand struct{ reply chan *SchedulerSnapshot }
type drainCommand struct {
	ctx   context.Context
	reply chan DrainReport
}
type drainWaiter struct {
	command drainCommand
	report  DrainReport
}

type Coordinator struct {
	opts      CoordinatorOptions
	commands  chan any
	closed    chan struct{}
	httpSlots chan struct{}
	closeOnce sync.Once
}

func NewCoordinator(opts CoordinatorOptions) *Coordinator {
	if opts.MaxHTTPSlots <= 0 {
		opts.MaxHTTPSlots = 1
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = legacyLeaseDuration
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.AfterFunc == nil {
		opts.AfterFunc = func(d time.Duration, fn func()) { time.AfterFunc(d, fn) }
	}
	c := &Coordinator{opts: opts, commands: make(chan any, 128), closed: make(chan struct{}), httpSlots: make(chan struct{}, opts.MaxHTTPSlots)}
	go c.loop()
	return c
}

func (c *Coordinator) Submit(intent Intent) Future[OperationResult] {
	reply := make(chan Future[OperationResult], 1)
	select {
	case c.commands <- submitCommand{intent, reply}:
	case <-c.closed:
		f := &futureState{done: make(chan struct{})}
		completeFuture(f, OperationResult{Err: errors.New("coordinator closed"), Disposition: ResultCancelled})
		return Future[OperationResult]{f}
	}
	return <-reply
}

func (c *Coordinator) PublishSnapshot() *SchedulerSnapshot {
	reply := make(chan *SchedulerSnapshot, 1)
	select {
	case c.commands <- snapshotCommand{reply}:
		return <-reply
	case <-c.closed:
		return &SchedulerSnapshot{}
	}
}
func (c *Coordinator) DrainLegacy(ctx context.Context) DrainReport {
	reply := make(chan DrainReport, 1)
	select {
	case c.commands <- drainCommand{ctx, reply}:
	case <-c.closed:
		return DrainReport{}
	}
	select {
	case r := <-reply:
		return r
	case <-ctx.Done():
		return DrainReport{TimedOut: true}
	}
}
func (c *Coordinator) Close() {
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = c.DrainLegacy(ctx)
		close(c.closed)
	})
}

func (c *Coordinator) loop() {
	accepting := true
	inflight := map[operationKey]*coordinatorJob{}
	leases := map[AuthInstanceID]*coordinatorJob{}
	queue := map[AuthInstanceID][]*coordinatorJob{}
	completedFence := map[AuthInstanceID]uint64{}
	var drains []*drainWaiter
	start := func(key operationKey, job *coordinatorJob) {
		ctx, cancel := context.WithCancel(context.Background())
		job.ctx, job.cancel = ctx, cancel
		job.held = &HeldLease{coordinator: c, intent: job.intent}
		leases[job.intent.Instance] = job
		c.opts.AfterFunc(c.opts.LeaseDuration, func() {
			select {
			case c.commands <- expireCommand{key, job}:
			case <-c.closed:
			}
		})
		go func() {
			res := OperationResult{Token: job.intent.Token, Generation: job.intent.Generation}
			if c.opts.Execute == nil {
				res.Err = fmt.Errorf("coordinator executor is not configured")
			} else {
				res = c.opts.Execute(ctx, job.intent, job.held)
			}
			select {
			case c.commands <- resultCommand{key, job, res}:
			case <-c.closed:
			}
		}()
	}
	var tryStart func(AuthInstanceID)
	tryStart = func(instance AuthInstanceID) {
		if leases[instance] != nil {
			return
		}
		q := queue[instance]
		for len(q) > 0 {
			job := q[0]
			if job.intent.StartedAfter > completedFence[instance] {
				queue[instance] = q
				return
			}
			q = q[1:]
			queue[instance] = q
			start(operationKey{job.intent.Instance, job.intent.Class, job.intent.Generation}, job)
			return
		}
		delete(queue, instance)
	}
	finishDrains := func() {
		if len(inflight) != 0 || len(drains) == 0 {
			return
		}
		for _, d := range drains {
			d.report.Completed++
			d.command.reply <- d.report
		}
		drains = nil
	}
	for {
		select {
		case <-c.closed:
			for _, job := range inflight {
				if job.cancel != nil {
					job.cancel()
				}
				completeFuture(job.future, OperationResult{Err: context.Canceled, Disposition: ResultCancelled})
			}
			return
		case raw := <-c.commands:
			switch cmd := raw.(type) {
			case submitCommand:
				key := operationKey{cmd.intent.Instance, cmd.intent.Class, cmd.intent.Generation}
				if existing := inflight[key]; existing != nil {
					cmd.reply <- Future[OperationResult]{existing.future}
					continue
				}
				f := &futureState{done: make(chan struct{})}
				if !accepting || cmd.intent.Source != LegacyRefreshSource {
					completeFuture(f, OperationResult{Err: errors.New("legacy coordinator is draining"), Disposition: ResultCancelled})
					cmd.reply <- Future[OperationResult]{f}
					continue
				}
				job := &coordinatorJob{intent: cmd.intent, future: f}
				inflight[key] = job
				queue[cmd.intent.Instance] = append(queue[cmd.intent.Instance], job)
				cmd.reply <- Future[OperationResult]{f}
				tryStart(cmd.intent.Instance)
			case expireCommand:
				if inflight[cmd.key] != cmd.job || cmd.job.expired {
					continue
				}
				cmd.job.expired = true
				if cmd.job.cancel != nil {
					cmd.job.cancel()
				}
			case resultCommand:
				if inflight[cmd.key] != cmd.job {
					continue
				}
				delete(inflight, cmd.key)
				if leases[cmd.job.intent.Instance] == cmd.job {
					delete(leases, cmd.job.intent.Instance)
				}
				probe, suppress := cmd.job.held.probeState()
				cmd.result.ProbeSendPhase = cmd.result.ProbeSendPhase || probe
				if suppress.After(cmd.result.SuppressUntil) {
					cmd.result.SuppressUntil = suppress
				}
				valid := !cmd.job.expired && cmd.result.Token == cmd.job.intent.Token
				if valid && c.opts.Validate != nil {
					valid = c.opts.Validate(cmd.job.intent, cmd.result)
				}
				if !valid {
					cmd.result.Disposition = ResultDiscardedStale
					for _, d := range drains {
						d.report.Invalidated++
					}
					if cmd.result.ProbeSendPhase && c.opts.InheritSentUnknown != nil {
						c.opts.InheritSentUnknown(cmd.job.intent, cmd.result.SuppressUntil)
					}
					if cmd.result.ProbeSendPhase {
						for _, d := range drains {
							d.report.SentUnknown++
						}
					}
				} else {
					cmd.result.Disposition = ResultApplied
					if cmd.result.Token.Fence > completedFence[cmd.job.intent.Instance] {
						completedFence[cmd.job.intent.Instance] = cmd.result.Token.Fence
					}
				}
				completeFuture(cmd.job.future, cmd.result)
				tryStart(cmd.job.intent.Instance)
				finishDrains()
			case snapshotCommand:
				queued := 0
				for _, q := range queue {
					queued += len(q)
				}
				ids := make([]AuthInstanceID, 0, len(leases))
				for id := range leases {
					ids = append(ids, id)
				}
				cmd.reply <- &SchedulerSnapshot{Accepting: accepting, InFlight: len(inflight), Queued: queued, LeasedInstances: ids, HTTPSlotsInUse: len(c.httpSlots)}
			case drainCommand:
				accepting = false
				waiter := &drainWaiter{command: cmd}
				for _, job := range inflight {
					if job.cancel != nil {
						job.cancel()
						job.expired = true
						waiter.report.Cancelled++
					}
				}
				if len(inflight) == 0 {
					waiter.report.Completed = 1
					cmd.reply <- waiter.report
				} else {
					drains = append(drains, waiter)
				}
			}
		}
	}
}
