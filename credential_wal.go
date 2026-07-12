package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrCredentialOutcomeUnknown = errors.New("credential save outcome unknown")
var ErrCredentialRejected = errors.New("credential save rejected")
var ErrStaleExecutionToken = errors.New("stale execution token")

type HostAuth struct {
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
	Ambiguous bool
	Phase     TransitionPhase
}
type CredentialManager struct {
	mu    sync.Mutex
	store StateWriter
	host  CredentialHost
	now   func() time.Time
	crash CrashHitter
	state PersistentState
}

func NewCredentialManager(store StateWriter, host CredentialHost, now func() time.Time, crash CrashHitter) *CredentialManager {
	if now == nil {
		now = time.Now
	}
	state := NewPersistentState()
	if ss, ok := store.(interface {
		PersistentSnapshot() (PersistentState, error)
	}); ok {
		if st, err := ss.PersistentSnapshot(); err == nil {
			state = st
		}
	}
	return &CredentialManager{store: store, host: host, now: now, crash: crash, state: state}
}
func (m *CredentialManager) SetChain(instance AuthInstanceID, chain TransitionChain) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.CredentialChains[instance] = chain
	if u, ok := m.store.(interface {
		Update(func(*PersistentState) error) error
	}); ok {
		_ = u.Update(func(s *PersistentState) error { s.CredentialChains[instance] = chain; return nil })
	}
}
func (m *CredentialManager) Chain(instance AuthInstanceID) TransitionChain {
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
		Update(func(*PersistentState) error) error
	}); ok {
		if err := u.Update(fn); err != nil {
			return err
		}
		if ss, ok := m.store.(interface {
			PersistentSnapshot() (PersistentState, error)
		}); ok {
			m.state, _ = ss.PersistentSnapshot()
		}
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if token.Instance != instance {
		return CredentialSaveResult{}, ErrStaleExecutionToken
	}
	chain, ok := m.state.CredentialChains[instance]
	if !ok {
		return CredentialSaveResult{}, errors.New("credential chain missing")
	}
	tr := CredentialTransition{Prev: chain.Tail(), Next: next.Fingerprint, Phase: TransitionPlanned, CreatedAt: m.now()}
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
	err := m.host.SaveAuth(ctx, instance, next)
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
	m.mu.Lock()
	defer m.mu.Unlock()
	chain := m.state.CredentialChains[instance]
	if len(chain.Transitions) == 0 {
		return CredentialRecoveryReport{}, nil
	}
	i := len(chain.Transitions) - 1
	tr := chain.Transitions[i]
	if tr.Phase != TransitionOutcomeUnknown && tr.Phase != TransitionPlanned {
		return CredentialRecoveryReport{Phase: tr.Phase}, nil
	}
	current, err := m.host.GetAuth(ctx, instance)
	if err != nil {
		return CredentialRecoveryReport{}, err
	}
	switch current.Fingerprint.CompositeHash {
	case tr.Next.CompositeHash:
		tr.Phase = TransitionApplied
	case tr.Prev.CompositeHash:
		tr.Phase = TransitionAborted
	default:
		return CredentialRecoveryReport{Ambiguous: true, Phase: tr.Phase}, nil
	}
	return CredentialRecoveryReport{Phase: tr.Phase}, m.mutate(func(s *PersistentState) error {
		ch := s.CredentialChains[instance]
		ch.Transitions[i] = tr
		s.CredentialChains[instance] = ch
		return nil
	})
}
