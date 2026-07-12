package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

type rosterCredentialHost struct {
	mu       sync.RWMutex
	host     HostClient
	bindings *BindingRegistry
	roster   HostRosterSnapshot
}

func (h *rosterCredentialHost) setRoster(roster HostRosterSnapshot) {
	h.mu.Lock()
	h.roster = roster
	h.mu.Unlock()
}

func (h *rosterCredentialHost) binding(instance AuthInstanceID) (RuntimeBinding, bool) {
	if h.bindings == nil {
		return RuntimeBinding{}, false
	}
	h.bindings.mu.RLock()
	defer h.bindings.mu.RUnlock()
	for _, b := range h.bindings.bindings {
		if b.Instance == instance {
			return b, true
		}
	}
	return RuntimeBinding{}, false
}
func (h *rosterCredentialHost) GetAuth(_ context.Context, instance AuthInstanceID) (HostAuth, error) {
	var b RuntimeBinding
	var ok bool
	h.mu.RLock()
	roster := h.roster
	h.mu.RUnlock()
	for _, entry := range roster.Entries {
		if legacyAuthInstanceID(entry.ID) == instance {
			b = RuntimeBinding{Instance: instance, AuthIndex: entry.AuthIndex}
			ok = true
			break
		}
	}
	if !ok {
		b, ok = h.binding(instance)
	}
	if !ok {
		return HostAuth{}, ErrBindingNotRosterConfirmed
	}
	resp, err := h.host.GetAuth(b.AuthIndex)
	if err != nil {
		return HostAuth{}, err
	}
	credentials, err := ExtractCodexCredentials(resp.JSON)
	if err != nil {
		return HostAuth{}, err
	}
	return HostAuth{Name: resp.Name, Raw: append([]byte(nil), resp.JSON...), Fingerprint: NewCredentialFingerprint(credentials.ChatGPTAccountID, credentials.RefreshToken, b.AuthIndex)}, nil
}
func (h *rosterCredentialHost) SaveAuth(_ context.Context, instance AuthInstanceID, auth HostAuth) error {
	b, ok := h.binding(instance)
	if !ok {
		return ErrBindingNotRosterConfirmed
	}
	if len(auth.Raw) == 0 || !json.Valid(auth.Raw) {
		return errors.New("credential save requires valid auth JSON")
	}
	name := b.AuthName
	if name == "" {
		name = b.AuthIndex
	}
	return h.host.SaveAuth(name, json.RawMessage(auth.Raw))
}
