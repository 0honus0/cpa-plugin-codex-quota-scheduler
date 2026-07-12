package main

import (
	"context"
	"errors"
	"sync"
)

var ErrCapabilityB = errors.New("authoritative host roster unavailable")
var ErrBindingNotRosterConfirmed = errors.New("binding is not roster-confirmed")

type RuntimeBinding struct {
	AuthID      string                 `json:"auth_id"`
	AuthIndex   string                 `json:"auth_index"`
	AuthName    string                 `json:"auth_name"`
	Instance    AuthInstanceID         `json:"instance"`
	Admission   InstanceAdmissionEpoch `json:"admission_epoch"`
	Generation  AuthBindingEpoch       `json:"binding_generation"`
	Login       LoginEpoch             `json:"login_epoch"`
	Token       TokenEpoch             `json:"token_epoch"`
	Fingerprint CredentialFingerprint  `json:"fingerprint"`
	AuthBlocked bool                   `json:"auth_blocked,omitempty"`
}

func (b RuntimeBinding) ExecutionToken(fence uint64) ExecutionToken {
	return ExecutionToken{Instance: b.Instance, Admission: b.Admission, Tier: TierGeneration(b.Generation), Fence: fence}
}

type BindingRegistry struct {
	mu       sync.RWMutex
	store    *StateStore
	bindings map[string]RuntimeBinding
}

func NewBindingRegistry(store *StateStore) (*BindingRegistry, error) {
	state, _, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &BindingRegistry{store: store, bindings: state.Bindings}, nil
}

func (r *BindingRegistry) Lookup(authID string) (RuntimeBinding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.bindings[authID]
	return b, ok
}

func (r *BindingRegistry) MarkAuthBlocked(authID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.bindings[authID]
	b.AuthID = authID
	b.AuthBlocked = true
	r.bindings[authID] = b
	_, _ = r.store.Update(func(s *PersistentState) error { s.Bindings[authID] = b; return nil })
	// Repeat the same semantic value so both primary and atomic backup carry
	// the fail-closed bit; loss of the primary cannot become an unlock path.
	_, _ = r.store.Update(func(s *PersistentState) error { s.Bindings[authID] = b; return nil })
}

// ObserveAuthoritative performs the INV-33 read-only genesis bootstrap. It
// accepts bindings only from the S1 authoritative roster snapshot.
func (r *BindingRegistry) ObserveAuthoritative(ctx context.Context, roster HostRosterSnapshot, authID string, host CredentialHost) (RuntimeBinding, bool, error) {
	if roster.Capability != CapabilityA {
		return RuntimeBinding{}, false, ErrCapabilityB
	}
	var entry *RosterEntry
	for i := range roster.Entries {
		if roster.Entries[i].ID == authID {
			entry = &roster.Entries[i]
			break
		}
	}
	if entry == nil || entry.AuthIndex == "" {
		return RuntimeBinding{}, false, ErrBindingNotRosterConfirmed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.bindings[authID]; ok && existing.Instance != 0 {
		return existing, false, nil
	}
	instance := legacyAuthInstanceID(authID)
	observed, err := host.GetAuth(ctx, instance)
	if err != nil {
		return RuntimeBinding{}, false, err
	}
	blocked := r.bindings[authID].AuthBlocked
	b := RuntimeBinding{AuthID: authID, AuthIndex: entry.AuthIndex, AuthName: observed.Name, Instance: instance, Admission: 1, Generation: 1, Login: 1, Token: 1, Fingerprint: observed.Fingerprint, AuthBlocked: blocked}
	committed, err := r.store.Update(func(s *PersistentState) error {
		s.Bindings[authID] = b
		if _, ok := s.CredentialChains[instance]; !ok {
			s.CredentialChains[instance] = TransitionChain{Cursor: observed.Fingerprint}
		}
		return nil
	})
	if err != nil {
		return RuntimeBinding{}, false, err
	}
	r.bindings = committed.Bindings
	return b, true, nil
}
