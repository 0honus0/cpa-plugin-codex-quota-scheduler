package main

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"
)

var ErrCredentialOutcomeUnknown = errors.New("credential save outcome unknown")
var ErrCredentialRejected = errors.New("credential save rejected")
var ErrCredentialUnresolved = errors.New("credential transition unresolved")
var ErrStaleExecutionToken = errors.New("stale execution token")
var ErrCredentialRollbackFailed = errors.New("credential rollback failed")

type HostAuth struct {
	Name        string
	Raw         []byte
	Fingerprint CredentialFingerprint
}
type CredentialHost interface {
	SaveAuth(context.Context, AuthInstanceID, HostAuth) error
	GetAuth(context.Context, AuthInstanceID) (HostAuth, error)
}
type StateWriter interface{ WriteThrough(PersistentState) error }
type CrashHitter interface{ Hit(string) error }
type CredentialSaveResult struct {
	Phase   TransitionPhase
	SaveSeq uint64
}
type CredentialRecoveryReport struct {
	Ambiguous   bool
	Phase       TransitionPhase
	Observation CredentialObservation
}
type CredentialReconcileHooks struct {
	BeforeCommit func(CredentialRecoveryReport) error
	AfterCommit  func(CredentialRecoveryReport, PersistentState) error
}
type CredentialManager struct {
	mu    sync.Mutex
	store StateWriter
	host  CredentialHost
	now   func() time.Time
	crash CrashHitter
	state PersistentState
}

func NewCredentialManager(store StateWriter, host CredentialHost, now func() time.Time, crash CrashHitter) (*CredentialManager, error) {
	if now == nil {
		now = time.Now
	}
	state := NewPersistentState()
	if ss, ok := store.(interface {
		PersistentSnapshot() (PersistentState, error)
	}); ok {
		st, err := ss.PersistentSnapshot()
		if err != nil {
			return nil, err
		}
		state = st
	}
	return &CredentialManager{store: store, host: host, now: now, crash: crash, state: state}, nil
}
func (m *CredentialManager) setChain(instance AuthInstanceID, chain TransitionChain) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.store.(interface {
		Update(func(*PersistentState) error) (PersistentState, error)
	}); ok {
		committed, err := u.Update(func(s *PersistentState) error { s.CredentialChains[instance] = chain; return nil })
		if err != nil {
			return err
		}
		m.state = committed
		return nil
	} else {
		next := clonePersistentState(m.state)
		next.CredentialChains[instance] = chain
		if err := m.store.WriteThrough(next); err != nil {
			return err
		}
	}
	m.state.CredentialChains[instance] = chain
	return nil
}
func (m *CredentialManager) chain(instance AuthInstanceID) TransitionChain {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.CredentialChains[instance]
}
func (m *CredentialManager) hit(name string) error {
	if m.crash != nil {
		return m.crash.Hit(name)
	}
	return nil
}
func (m *CredentialManager) mutate(fn func(*PersistentState) error) error {
	if u, ok := m.store.(interface {
		Update(func(*PersistentState) error) (PersistentState, error)
	}); ok {
		committed, err := u.Update(fn)
		if err != nil {
			return err
		}
		m.state = committed
		return nil
	}
	next := clonePersistentState(m.state)
	if err := fn(&next); err != nil {
		return err
	}
	if err := m.store.WriteThrough(next); err != nil {
		return err
	}
	m.state = next
	return nil
}
func (m *CredentialManager) SaveVersioned(ctx context.Context, instance AuthInstanceID, next HostAuth, token ExecutionToken) (CredentialSaveResult, error) {
	return m.saveVersionedWithHost(ctx, instance, next, token, m.host)
}
func (m *CredentialManager) SaveVersionedWithHost(ctx context.Context, instance AuthInstanceID, next HostAuth, token ExecutionToken, host CredentialHost) (CredentialSaveResult, error) {
	return m.saveVersionedWithHost(ctx, instance, next, token, host)
}
func (m *CredentialManager) saveVersionedWithHost(ctx context.Context, instance AuthInstanceID, next HostAuth, token ExecutionToken, host CredentialHost) (CredentialSaveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ss, ok := m.store.(interface {
		PersistentSnapshot() (PersistentState, error)
	}); ok {
		fresh, err := ss.PersistentSnapshot()
		if err != nil {
			return CredentialSaveResult{}, err
		}
		m.state = fresh
	}
	if token.Instance != instance {
		return CredentialSaveResult{}, ErrStaleExecutionToken
	}
	chain, ok := m.state.CredentialChains[instance]
	if !ok {
		return CredentialSaveResult{}, errors.New("credential chain missing")
	}
	tail, tailErr := chain.SaveTail()
	if tailErr != nil {
		return CredentialSaveResult{}, tailErr
	}
	tr := CredentialTransition{Prev: tail, Next: next.Fingerprint, Phase: TransitionPlanned, CreatedAt: m.now()}
	//kpoint:K_CREDENTIAL_PLANNED_WRITE
	if err := m.hit("K_CREDENTIAL_PLANNED_WRITE"); err != nil {
		return CredentialSaveResult{}, err
	}
	if err := m.mutate(func(s *PersistentState) error {
		s.NextSaveSeq++
		tr.SaveSeq = s.NextSaveSeq
		ch := s.CredentialChains[instance]
		ch = ch.AppendAt(tr, m.now())
		s.CredentialChains[instance] = ch
		return nil
	}); err != nil {
		return CredentialSaveResult{}, err
	}
	//kpoint:K_CREDENTIAL_AFTER_PLANNED
	if err := m.hit("K_CREDENTIAL_AFTER_PLANNED"); err != nil {
		return CredentialSaveResult{TransitionPlanned, tr.SaveSeq}, err
	}
	//kpoint:K_CREDENTIAL_BEFORE_SAVEAUTH
	if err := m.hit("K_CREDENTIAL_BEFORE_SAVEAUTH"); err != nil {
		return CredentialSaveResult{TransitionPlanned, tr.SaveSeq}, err
	}
	err := host.SaveAuth(ctx, instance, next)
	//kpoint:K_CREDENTIAL_AFTER_SAVEAUTH
	if hitErr := m.hit("K_CREDENTIAL_AFTER_SAVEAUTH"); hitErr != nil {
		return CredentialSaveResult{TransitionOutcomeUnknown, tr.SaveSeq}, hitErr
	}
	phase := TransitionApplied
	if err != nil {
		phase = TransitionOutcomeUnknown
		if errors.Is(err, ErrCredentialRejected) {
			phase = TransitionAborted
		}
	}
	//kpoint:K_CREDENTIAL_BEFORE_TERMINAL
	if hitErr := m.hit("K_CREDENTIAL_BEFORE_TERMINAL"); hitErr != nil {
		return CredentialSaveResult{TransitionOutcomeUnknown, tr.SaveSeq}, hitErr
	}
	//kpoint:K_CREDENTIAL_TERMINAL_WRITE
	if hitErr := m.hit("K_CREDENTIAL_TERMINAL_WRITE"); hitErr != nil {
		return CredentialSaveResult{TransitionOutcomeUnknown, tr.SaveSeq}, hitErr
	}
	if persistErr := m.mutate(func(s *PersistentState) error {
		ch := s.CredentialChains[instance]
		for i := range ch.Transitions {
			if ch.Transitions[i].SaveSeq == tr.SaveSeq {
				ch.Transitions[i].Phase = phase
			}
		}
		s.CredentialChains[instance] = ch
		return nil
	}); persistErr != nil {
		return CredentialSaveResult{phase, tr.SaveSeq}, persistErr
	}
	return CredentialSaveResult{phase, tr.SaveSeq}, err
}
func (m *CredentialManager) Reconcile(ctx context.Context, instance AuthInstanceID) (CredentialRecoveryReport, error) {
	return m.reconcile(ctx, instance, false, CredentialReconcileHooks{})
}

func (m *CredentialManager) ReconcileWithHooks(ctx context.Context, instance AuthInstanceID, hooks CredentialReconcileHooks) (CredentialRecoveryReport, error) {
	return m.reconcile(ctx, instance, false, hooks)
}

func (m *CredentialManager) ReconcileUnresolved(ctx context.Context, instance AuthInstanceID) (CredentialRecoveryReport, error) {
	return m.reconcile(ctx, instance, true, CredentialReconcileHooks{})
}

func (m *CredentialManager) reconcile(ctx context.Context, instance AuthInstanceID, requireUnresolved bool, hooks CredentialReconcileHooks) (CredentialRecoveryReport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ss, ok := m.store.(interface {
		PersistentSnapshot() (PersistentState, error)
	}); ok {
		fresh, err := ss.PersistentSnapshot()
		if err != nil {
			return CredentialRecoveryReport{}, err
		}
		m.state = fresh
	}
	chain, ok := m.state.CredentialChains[instance]
	if !ok || !chain.Cursor.ValidHashes() {
		if requireUnresolved {
			return CredentialRecoveryReport{}, ErrCredentialResolutionScope
		}
		return CredentialRecoveryReport{}, nil
	}
	if requireUnresolved && !credentialChainUnresolved(chain) {
		return CredentialRecoveryReport{}, ErrCredentialResolutionScope
	}
	current, err := m.host.GetAuth(ctx, instance)
	if err != nil {
		return CredentialRecoveryReport{}, err
	}
	nextChain, report := reconcileCredentialObservation(chain, current.Fingerprint, m.now())
	if report.Ambiguous {
		return report, nil
	}
	if hooks.BeforeCommit != nil {
		if err := hooks.BeforeCommit(report); err != nil {
			return report, err
		}
	}
	before := clonePersistentState(m.state)
	err = m.mutate(func(s *PersistentState) error {
		if requireUnresolved && !credentialChainUnresolved(s.CredentialChains[instance]) {
			return ErrCredentialResolutionScope
		}
		s.CredentialChains[instance] = nextChain
		for authID, binding := range s.Bindings {
			if binding.Instance != instance {
				continue
			}
			switch report.Observation.Kind {
			case CredentialOwnedRotation:
				binding.Token++
				binding.Fingerprint = current.Fingerprint
				s.Bindings[authID] = binding
			case CredentialExternalLogin:
				binding.Login++
				binding.Admission++
				binding.Fingerprint = current.Fingerprint
				binding.AuthBlocked = false
				s.TierGeneration++
				if s.TierGeneration == 0 {
					s.TierGeneration = 1
				}
				if s.AdmissionEpochs == nil {
					s.AdmissionEpochs = map[AuthInstanceID]InstanceAdmissionEpoch{}
				}
				s.AdmissionEpochs[instance] = binding.Admission
				binding.Generation = AuthBindingEpoch(s.TierGeneration)
				for id, active := range s.Bindings {
					active.Generation = AuthBindingEpoch(s.TierGeneration)
					s.Bindings[id] = active
				}
				s.Bindings[authID] = binding
			}
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	committed := clonePersistentState(m.state)
	if hooks.AfterCommit != nil {
		if afterErr := hooks.AfterCommit(report, committed); afterErr != nil {
			rollbackErr := m.mutate(func(s *PersistentState) error {
				if s.TierGeneration != committed.TierGeneration || !reflect.DeepEqual(s.AdmissionEpochs, committed.AdmissionEpochs) || !reflect.DeepEqual(s.CredentialChains, committed.CredentialChains) || !reflect.DeepEqual(s.Bindings, committed.Bindings) {
					return ErrCredentialResolutionScope
				}
				s.TierGeneration = before.TierGeneration
				s.AdmissionEpochs = clonePersistentState(before).AdmissionEpochs
				s.CredentialChains = clonePersistentState(before).CredentialChains
				s.Bindings = clonePersistentState(before).Bindings
				return nil
			})
			if rollbackErr != nil {
				return report, errors.Join(afterErr, ErrCredentialRollbackFailed, rollbackErr)
			}
			return report, afterErr
		}
	}
	return report, nil
}

func reconcileCredentialObservation(chain TransitionChain, observed CredentialFingerprint, now time.Time) (TransitionChain, CredentialRecoveryReport) {
	report := CredentialRecoveryReport{}
	if len(chain.Transitions) > 0 {
		i := len(chain.Transitions) - 1
		transition := chain.Transitions[i]
		if transition.Phase == TransitionPlanned || transition.Phase == TransitionOutcomeUnknown {
			report.Phase = transition.Phase
			switch observed.CompositeHash {
			case transition.Next.CompositeHash:
				transition.Phase = TransitionApplied
				report.Phase = TransitionApplied
			case transition.Prev.CompositeHash:
				transition.Phase = TransitionAborted
				report.Phase = TransitionAborted
			default:
				report.Ambiguous = true
				return chain, report
			}
			chain.Transitions[i] = transition
		} else {
			report.Phase = transition.Phase
		}
	}
	report.Observation = ClassifyObservedCredentialAt(chain, observed, now)
	switch report.Observation.Kind {
	case CredentialOwnedRotation:
		chain.Cursor = observed
		chain.Transitions = append([]CredentialTransition(nil), chain.Transitions[report.Observation.Advance:]...)
	case CredentialExternalLogin:
		chain = TransitionChain{Cursor: observed}
	}
	for len(chain.Transitions) > 0 && chain.Transitions[0].Prev == chain.Cursor && chain.Transitions[0].Phase == TransitionAborted {
		chain.Transitions = chain.Transitions[1:]
	}
	return chain, report
}
