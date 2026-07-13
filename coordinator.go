package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type OperationClass string

const (
	OperationLegacyRefresh OperationClass = "legacy_refresh"
	OperationQuotaRead     OperationClass = "quota_read"
	OperationProbePrecheck OperationClass = "probe_precheck"
	OperationProbeVerify   OperationClass = "probe_verify"
	OperationProbeSequence OperationClass = "probe_sequence"
	// Retained only for the held S3 normal-refresh adapter; typed Probe cannot
	// submit this source. Its wire value stays stable for the unmodified S3 ABI.
	LegacyEnvelopeSource         = "legacy_refresh_txn"
	LegacyRefreshSource          = LegacyEnvelopeSource
	SourceSchedulerInitial       = "scheduler_initial"
	SourceSchedulerInterval      = "scheduler_interval"
	SourceSchedulerStaleRecovery = "scheduler_stale_recovery"
	SourceManualRefresh          = "manual_refresh"
	SourceProbeStartup           = "probe_startup"
	SourceProbePrecheck          = "probe_precheck"
	SourceProbeActivation        = "probe_activation"
	SourceProbeVerify            = "probe_verify"
	legacyLeaseDuration          = 2 * time.Minute
)

type Intent struct {
	AuthID       string
	Instance     AuthInstanceID
	Generation   TierGeneration
	Class        OperationClass
	Source       string
	StartedAfter uint64
	ReadStartSeq uint64
	AttemptID    string
	Token        ExecutionToken
	Login        LoginEpoch
	Fingerprint  CredentialFingerprint
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
	ReadStartSeq   uint64
	Disposition    ResultDisposition
	Journal        *LegacyEffectJournal
	CallbackFailed bool
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
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case h.coordinator.httpSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-h.coordinator.httpSlots }()
	if err := ctx.Err(); err != nil {
		return err
	}
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

func (h *HeldLease) WaitPropagation(ctx context.Context, d time.Duration) error {
	if h.coordinator.opts.PropagationWait != nil {
		return h.coordinator.opts.PropagationWait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	AllocateReadSeq    func() (uint64, error)
	PropagationWait    func(context.Context, time.Duration) error
	Execute            func(context.Context, Intent, *HeldLease) OperationResult
	Validate           func(Intent, OperationResult) bool
	Apply              func(Intent, OperationResult) error
	InheritSentUnknown func(Intent, time.Time) error
	DispatchLogs       func(context.Context, Intent, OperationResult, *HeldLease) error
}

type CoordinatorSnapshot struct {
	Accepting       bool
	InFlight        int
	Queued          int
	LeasedInstances []AuthInstanceID
	HTTPSlotsInUse  int
}

type DrainReport struct {
	Completed, Cancelled, Invalidated, SentUnknown int
	TimedOut                                       bool
	PersistenceFailures                            int
	HandoffBlocked                                 bool
}

type operationKey struct {
	instance   AuthInstanceID
	class      OperationClass
	generation TierGeneration
	attemptID  string
	nonce      uint64
}
type coordinatorJob struct {
	key         operationKey
	intent      Intent
	future      *futureState
	ctx         context.Context
	cancel      context.CancelFunc
	held        *HeldLease
	expired     bool
	invalidated bool
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
type logCallbackCommand struct {
	key    operationKey
	job    *coordinatorJob
	result OperationResult
	err    error
}
type snapshotCommand struct{ reply chan *CoordinatorSnapshot }
type drainCommand struct {
	ctx   context.Context
	reply chan DrainReport
}
type cancelInstancesCommand struct {
	instances  []AuthInstanceID
	reply      chan struct{}
	tokenReply chan map[AuthInstanceID]uint64
}
type activateInstancesCommand struct {
	generations map[AuthInstanceID]TierGeneration
	reply       chan struct{}
}
type restoreInstancesCommand struct {
	generations map[AuthInstanceID]TierGeneration
	tokens      map[AuthInstanceID]uint64
	reply       chan struct{}
}
type drainWaiter struct {
	command drainCommand
	report  DrainReport
}

type Coordinator struct {
	opts        CoordinatorOptions
	commands    chan any
	closed      chan struct{}
	httpSlots   chan struct{}
	closeOnce   sync.Once
	lifecycleMu sync.Mutex
	closing     bool
	typedMu     sync.Mutex
	typedReads  map[AuthInstanceID][]*typedReadEntry
	typedNonce  atomic.Uint64
}

type typedReadEntry struct {
	future    Future[OperationResult]
	seq       atomic.Uint64
	done      atomic.Bool
	attemptID string
	class     OperationClass
}
type typedPayload struct {
	value any
	entry *typedReadEntry
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
	c := &Coordinator{opts: opts, commands: make(chan any, 128), closed: make(chan struct{}), httpSlots: make(chan struct{}, opts.MaxHTTPSlots), typedReads: map[AuthInstanceID][]*typedReadEntry{}}
	go c.loop()
	return c
}

func (c *Coordinator) SubmitTyped(intent Intent) Future[OperationResult] {
	if isTypedRead(intent.Class) {
		c.typedMu.Lock()
		entries := c.typedReads[intent.Instance][:0]
		for _, entry := range c.typedReads[intent.Instance] {
			if !entry.done.Load() {
				entries = append(entries, entry)
			}
		}
		c.typedReads[intent.Instance] = entries
		for _, entry := range entries {
			seq := entry.seq.Load()
			attemptOK := true
			if intent.Class == OperationProbeVerify {
				attemptOK = entry.attemptID == "" || entry.attemptID == intent.AttemptID
			}
			queuedSameClass := seq == 0 && intent.StartedAfter == 0 && entry.class == intent.Class
			if !entry.done.Load() && (seq > intent.StartedAfter || queuedSameClass) && attemptOK {
				if intent.Class == OperationProbeVerify && entry.attemptID == "" {
					entry.attemptID = intent.AttemptID
				}
				f := entry.future
				c.typedMu.Unlock()
				return f
			}
		}
		entry := &typedReadEntry{attemptID: intent.AttemptID, class: intent.Class}
		intent.AttemptID = fmt.Sprintf("read-%d", c.typedNonce.Add(1))
		intent.Payload = typedPayload{value: intent.Payload, entry: entry}
		f := c.Submit(intent)
		entry.future = f
		c.typedReads[intent.Instance] = append(c.typedReads[intent.Instance], entry)
		c.typedMu.Unlock()
		return f
	}
	if intent.Class == OperationProbeSend {
		intent.AttemptID = fmt.Sprintf("%s#%d", intent.AttemptID, c.typedNonce.Add(1))
	}
	return c.Submit(intent)
}

func isTypedRead(class OperationClass) bool {
	return class == OperationQuotaRead || class == OperationProbePrecheck || class == OperationProbeVerify
}
func formalSource(source string) bool {
	switch source {
	case LegacyEnvelopeSource, SourceSchedulerInitial, SourceSchedulerInterval, SourceSchedulerStaleRecovery, SourceManualRefresh, SourceProbeStartup, SourceProbePrecheck, SourceProbeActivation, SourceProbeVerify:
		return true
	}
	return false
}

func (c *Coordinator) Submit(intent Intent) Future[OperationResult] {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closing {
		f := &futureState{done: make(chan struct{})}
		completeFuture(f, OperationResult{Err: errors.New("coordinator closed"), Disposition: ResultCancelled})
		return Future[OperationResult]{f}
	}
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

func (c *Coordinator) PublishSnapshot() *CoordinatorSnapshot {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closing {
		return &CoordinatorSnapshot{}
	}
	reply := make(chan *CoordinatorSnapshot, 1)
	select {
	case c.commands <- snapshotCommand{reply}:
		return <-reply
	case <-c.closed:
		return &CoordinatorSnapshot{}
	}
}
func (c *Coordinator) CancelInstances(instances []AuthInstanceID) {
	if c == nil || len(instances) == 0 {
		return
	}
	c.lifecycleMu.Lock()
	if c.closing {
		c.lifecycleMu.Unlock()
		return
	}
	reply := make(chan struct{}, 1)
	ids := append([]AuthInstanceID(nil), instances...)
	select {
	case c.commands <- cancelInstancesCommand{instances: ids, reply: reply}:
		<-reply
	case <-c.closed:
		c.lifecycleMu.Unlock()
		return
	}
	c.lifecycleMu.Unlock()
	c.clearTypedReads(ids)
}
func (c *Coordinator) clearTypedReads(ids []AuthInstanceID) {
	c.typedMu.Lock()
	for _, instance := range ids {
		for _, entry := range c.typedReads[instance] {
			entry.done.Store(true)
		}
		delete(c.typedReads, instance)
	}
	c.typedMu.Unlock()
}
func (c *Coordinator) suspendInstancesForRollback(instances []AuthInstanceID) map[AuthInstanceID]uint64 {
	if c == nil || len(instances) == 0 {
		return nil
	}
	c.lifecycleMu.Lock()
	if c.closing {
		c.lifecycleMu.Unlock()
		return nil
	}
	reply := make(chan map[AuthInstanceID]uint64, 1)
	ids := append([]AuthInstanceID(nil), instances...)
	select {
	case c.commands <- cancelInstancesCommand{instances: ids, tokenReply: reply}:
		tokens := <-reply
		c.lifecycleMu.Unlock()
		c.clearTypedReads(ids)
		return tokens
	case <-c.closed:
		c.lifecycleMu.Unlock()
		return nil
	}
}
func (c *Coordinator) activateInstances(generations map[AuthInstanceID]TierGeneration) {
	if c == nil || len(generations) == 0 {
		return
	}
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closing {
		return
	}
	copyGenerations := make(map[AuthInstanceID]TierGeneration, len(generations))
	for instance, generation := range generations {
		copyGenerations[instance] = generation
	}
	reply := make(chan struct{}, 1)
	select {
	case c.commands <- activateInstancesCommand{generations: copyGenerations, reply: reply}:
		<-reply
	case <-c.closed:
	}
}
func (c *Coordinator) restoreCancelledInstances(generations map[AuthInstanceID]TierGeneration, tokens map[AuthInstanceID]uint64) {
	if c == nil || len(generations) == 0 {
		return
	}
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closing {
		return
	}
	copyGenerations := make(map[AuthInstanceID]TierGeneration, len(generations))
	for instance, generation := range generations {
		copyGenerations[instance] = generation
	}
	reply := make(chan struct{}, 1)
	select {
	case c.commands <- restoreInstancesCommand{generations: copyGenerations, tokens: tokens, reply: reply}:
		<-reply
	case <-c.closed:
	}
}
func (c *Coordinator) DoHostCallback(ctx context.Context, call func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.httpSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.httpSlots }()
	if err := ctx.Err(); err != nil {
		return err
	}
	call()
	return nil
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
		c.lifecycleMu.Lock()
		c.closing = true
		c.lifecycleMu.Unlock()
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
	instanceGeneration := map[AuthInstanceID]TierGeneration{}
	cancelledThrough := map[AuthInstanceID]TierGeneration{}
	cancelTokens := map[AuthInstanceID]uint64{}
	var cancelNonce uint64
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
			var readEntry *typedReadEntry
			if payload, ok := job.intent.Payload.(typedPayload); ok {
				readEntry = payload.entry
				job.intent.Payload = payload.value
				if c.opts.AllocateReadSeq != nil {
					seq, err := c.opts.AllocateReadSeq()
					if err != nil {
						select {
						case c.commands <- resultCommand{key: key, job: job, result: OperationResult{Token: job.intent.Token, Err: err}}:
						case <-c.closed:
						}
						return
					}
					job.intent.ReadStartSeq = seq
					readEntry.seq.Store(seq)
				}
			}
			res := OperationResult{Token: job.intent.Token, Generation: job.intent.Generation}
			if c.opts.Execute == nil {
				res.Err = fmt.Errorf("coordinator executor is not configured")
			} else {
				res = c.opts.Execute(ctx, job.intent, job.held)
			}
			res.ReadStartSeq = job.intent.ReadStartSeq
			if readEntry != nil {
				readEntry.done.Store(true)
			}
			select {
			case c.commands <- resultCommand{key, job, res}:
			case <-c.closed:
			}
		}()
	}
	var tryStart func(AuthInstanceID)
	tryStart = func(instance AuthInstanceID) {
		if !accepting {
			return
		}
		if leases[instance] != nil {
			return
		}
		q := queue[instance]
		for len(q) > 0 {
			job := q[0]
			// completedFence gates the S3 legacy execution-token queue only.
			// Typed reads use the distinct actual ReadStartSeq barrier.
			if job.intent.Class == OperationLegacyRefresh && job.intent.StartedAfter > completedFence[instance] {
				queue[instance] = q
				return
			}
			q = q[1:]
			queue[instance] = q
			start(job.key, job)
			return
		}
		delete(queue, instance)
	}
	finishDrains := func() {
		if len(inflight) != 0 || len(drains) == 0 {
			return
		}
		for _, d := range drains {
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
				nonce := uint64(0)
				if cmd.intent.Class == OperationProbeSend {
					nonce = c.typedNonce.Add(1)
				}
				key := operationKey{instance: cmd.intent.Instance, class: cmd.intent.Class, generation: cmd.intent.Generation, attemptID: cmd.intent.AttemptID, nonce: nonce}
				if existing := inflight[key]; existing != nil {
					cmd.reply <- Future[OperationResult]{existing.future}
					continue
				}
				f := &futureState{done: make(chan struct{})}
				_, cancelled := cancelledThrough[cmd.intent.Instance]
				if !accepting || cancelled || !formalSource(cmd.intent.Source) || (cmd.intent.Source == LegacyEnvelopeSource && cmd.intent.Class != OperationLegacyRefresh) {
					completeFuture(f, OperationResult{Err: errors.New("legacy coordinator is draining"), Disposition: ResultCancelled})
					cmd.reply <- Future[OperationResult]{f}
					continue
				}
				if cmd.intent.Generation > instanceGeneration[cmd.intent.Instance] {
					instanceGeneration[cmd.intent.Instance] = cmd.intent.Generation
				}
				job := &coordinatorJob{key: key, intent: cmd.intent, future: f}
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
				delete(inflight, cmd.key)
				if leases[cmd.job.intent.Instance] == cmd.job {
					delete(leases, cmd.job.intent.Instance)
				}
				probe, suppress := cmd.job.held.probeState()
				var inheritErr error
				if probe && c.opts.InheritSentUnknown != nil {
					inheritErr = c.opts.InheritSentUnknown(cmd.job.intent, suppress)
				}
				resultErr := error(context.DeadlineExceeded)
				if inheritErr != nil {
					resultErr = inheritErr
				}
				completeFuture(cmd.job.future, OperationResult{Token: cmd.job.intent.Token, ProbeSendPhase: probe, SuppressUntil: suppress, Err: resultErr, Disposition: ResultDiscardedStale})
				if cmd.job.intent.Token.Fence > completedFence[cmd.job.intent.Instance] {
					completedFence[cmd.job.intent.Instance] = cmd.job.intent.Token.Fence
				}
				for _, d := range drains {
					d.report.Completed++
					d.report.Invalidated++
					if probe && inheritErr == nil {
						d.report.SentUnknown++
					}
					if inheritErr != nil {
						d.report.PersistenceFailures++
						d.report.HandoffBlocked = true
					}
				}
				tryStart(cmd.job.intent.Instance)
				finishDrains()
			case resultCommand:
				if inflight[cmd.key] != cmd.job {
					continue
				}
				probe, suppress := cmd.job.held.probeState()
				cmd.result.ProbeSendPhase = cmd.result.ProbeSendPhase || probe
				if suppress.After(cmd.result.SuppressUntil) {
					cmd.result.SuppressUntil = suppress
				}
				valid := !cmd.job.expired && !cmd.job.invalidated && !cmd.result.CallbackFailed && cmd.result.Token == cmd.job.intent.Token
				if valid && c.opts.Validate != nil {
					valid = c.opts.Validate(cmd.job.intent, cmd.result)
				}
				if valid && c.opts.DispatchLogs != nil && cmd.result.Journal != nil && len(cmd.result.Journal.HostLogs) > 0 {
					logsResult := cmd.result
					go func() {
						err := c.opts.DispatchLogs(cmd.job.ctx, cmd.job.intent, logsResult, cmd.job.held)
						if logsResult.Journal != nil {
							copyJournal := *logsResult.Journal
							copyJournal.HostLogs = nil
							logsResult.Journal = &copyJournal
						}
						select {
						case c.commands <- logCallbackCommand{cmd.key, cmd.job, logsResult, err}:
						case <-c.closed:
						}
					}()
					continue
				}
				delete(inflight, cmd.key)
				if leases[cmd.job.intent.Instance] == cmd.job {
					delete(leases, cmd.job.intent.Instance)
				}
				if valid && c.opts.Apply != nil {
					if err := c.opts.Apply(cmd.job.intent, cmd.result); err != nil {
						cmd.result.Err = err
						valid = false
					}
				}
				if !valid {
					cmd.result.Disposition = ResultDiscardedStale
					if cmd.job.intent.Token.Fence > completedFence[cmd.job.intent.Instance] {
						completedFence[cmd.job.intent.Instance] = cmd.job.intent.Token.Fence
					}
					for _, d := range drains {
						d.report.Invalidated++
					}
					var inheritErr error
					if cmd.result.ProbeSendPhase && c.opts.InheritSentUnknown != nil {
						inheritErr = c.opts.InheritSentUnknown(cmd.job.intent, cmd.result.SuppressUntil)
					}
					if cmd.result.ProbeSendPhase && inheritErr == nil {
						for _, d := range drains {
							d.report.SentUnknown++
						}
					}
					if inheritErr != nil {
						cmd.result.Err = inheritErr
						for _, d := range drains {
							d.report.PersistenceFailures++
							d.report.HandoffBlocked = true
						}
					}
				} else {
					cmd.result.Disposition = ResultApplied
					if cmd.result.Token.Fence > completedFence[cmd.job.intent.Instance] {
						completedFence[cmd.job.intent.Instance] = cmd.result.Token.Fence
					}
				}
				completeFuture(cmd.job.future, cmd.result)
				for _, d := range drains {
					d.report.Completed++
				}
				tryStart(cmd.job.intent.Instance)
				finishDrains()
			case logCallbackCommand:
				if inflight[cmd.key] != cmd.job {
					continue
				}
				cmd.result.CallbackFailed = cmd.err != nil
				if cmd.err != nil {
					cmd.result.Err = cmd.err
				}
				c.commands <- resultCommand{key: cmd.key, job: cmd.job, result: cmd.result}
			case snapshotCommand:
				queued := 0
				for _, q := range queue {
					queued += len(q)
				}
				ids := make([]AuthInstanceID, 0, len(leases))
				for id := range leases {
					ids = append(ids, id)
				}
				cmd.reply <- &CoordinatorSnapshot{Accepting: accepting, InFlight: len(inflight), Queued: queued, LeasedInstances: ids, HTTPSlotsInUse: len(c.httpSlots)}
			case cancelInstancesCommand:
				issuedTokens := make(map[AuthInstanceID]uint64, len(cmd.instances))
				for _, instance := range cmd.instances {
					cancelNonce++
					cancelTokens[instance] = cancelNonce
					issuedTokens[instance] = cancelNonce
					cancelledThrough[instance] = instanceGeneration[instance]
					for _, job := range queue[instance] {
						if inflight[job.key] == job {
							delete(inflight, job.key)
						}
						job.invalidated = true
						completeFuture(job.future, OperationResult{Token: job.intent.Token, Err: context.Canceled, Disposition: ResultCancelled})
					}
					delete(queue, instance)
					if job := leases[instance]; job != nil {
						job.invalidated = true
						if job.cancel != nil {
							job.cancel()
						}
						delete(inflight, job.key)
						delete(leases, instance)
						completeFuture(job.future, OperationResult{Token: job.intent.Token, Err: context.Canceled, Disposition: ResultCancelled})
					}
				}
				if cmd.tokenReply != nil {
					cmd.tokenReply <- issuedTokens
				} else {
					cmd.reply <- struct{}{}
				}
				finishDrains()
			case activateInstancesCommand:
				for instance, generation := range cmd.generations {
					if generation > instanceGeneration[instance] {
						instanceGeneration[instance] = generation
					}
					if generation > cancelledThrough[instance] {
						delete(cancelledThrough, instance)
						delete(cancelTokens, instance)
					}
				}
				cmd.reply <- struct{}{}
			case restoreInstancesCommand:
				for instance, generation := range cmd.generations {
					if cancelledThrough[instance] == generation && cancelTokens[instance] == cmd.tokens[instance] {
						delete(cancelledThrough, instance)
						delete(cancelTokens, instance)
					}
				}
				cmd.reply <- struct{}{}
			case drainCommand:
				accepting = false
				waiter := &drainWaiter{command: cmd}
				for instance, jobs := range queue {
					for _, job := range jobs {
						key := job.key
						if inflight[key] == job {
							delete(inflight, key)
						}
						completeFuture(job.future, OperationResult{Token: job.intent.Token, Err: context.Canceled, Disposition: ResultCancelled})
						waiter.report.Cancelled++
						waiter.report.Completed++
					}
					delete(queue, instance)
				}
				for _, job := range inflight {
					if job.cancel != nil {
						job.cancel()
						job.invalidated = true
						waiter.report.Cancelled++
					}
				}
				if len(inflight) == 0 {
					cmd.reply <- waiter.report
				} else {
					drains = append(drains, waiter)
				}
			}
		}
	}
}
