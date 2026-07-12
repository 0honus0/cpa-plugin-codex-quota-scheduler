package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrCredentialOutcomeUnknown = errors.New("credential save outcome unknown")
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
	return &CredentialManager{store: store, host: host, now: now, crash: crash, state: NewPersistentState()}
}
func (m *CredentialManager) SetChain(instance AuthInstanceID, chain TransitionChain) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.CredentialChains[instance] = chain
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
func (m *CredentialManager) persist() error {
	return m.store.WriteThrough(clonePersistentState(m.state))
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
	m.state.NextSaveSeq++
	tr := CredentialTransition{Prev: chain.Tail(), Next: next.Fingerprint, SaveSeq: m.state.NextSaveSeq, Phase: TransitionPlanned, CreatedAt: m.now()}
	chain = chain.Append(tr)
	m.state.CredentialChains[instance] = chain
	//kpoint:K_CREDENTIAL_PLANNED_WRITE
	if err := m.persist(); err != nil {
		return CredentialSaveResult{}, err
	}
	if err := m.hit("K_CREDENTIAL_AFTER_PLANNED"); err != nil {
		return CredentialSaveResult{Phase: TransitionPlanned, SaveSeq: tr.SaveSeq}, err
	}
	//kpoint:K_CREDENTIAL_BEFORE_SAVEAUTH
	err := m.host.SaveAuth(ctx, instance, next)
	//kpoint:K_CREDENTIAL_AFTER_SAVEAUTH
	phase := TransitionApplied
	if err != nil {
		phase = TransitionAborted
		if errors.Is(err, ErrCredentialOutcomeUnknown) {
			phase = TransitionOutcomeUnknown
		}
	}
	chain = m.state.CredentialChains[instance]
	chain.Transitions[len(chain.Transitions)-1].Phase = phase
	m.state.CredentialChains[instance] = chain
	if hitErr := m.hit("K_CREDENTIAL_BEFORE_TERMINAL"); hitErr != nil {
		return CredentialSaveResult{Phase: TransitionOutcomeUnknown, SaveSeq: tr.SaveSeq}, hitErr
	}
	//kpoint:K_CREDENTIAL_TERMINAL_WRITE
	if persistErr := m.persist(); persistErr != nil {
		return CredentialSaveResult{Phase: phase, SaveSeq: tr.SaveSeq}, persistErr
	}
	return CredentialSaveResult{Phase: phase, SaveSeq: tr.SaveSeq}, err
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
	chain.Transitions[i] = tr
	m.state.CredentialChains[instance] = chain
	return CredentialRecoveryReport{Phase: tr.Phase}, m.persist()
}
