package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const CurrentStateSchema = 1

var ErrStateReadOnly = errors.New("state store is read-only")

type PersistentState struct {
	SchemaVersion    int                                `json:"schema_version"`
	ReservedCeiling  uint64                             `json:"reserved_ceiling"`
	NextSaveSeq      uint64                             `json:"next_save_seq"`
	CredentialChains map[AuthInstanceID]TransitionChain `json:"credential_chains,omitempty"`
}

func NewPersistentState() PersistentState {
	return PersistentState{SchemaVersion: CurrentStateSchema, CredentialChains: map[AuthInstanceID]TransitionChain{}}
}
func clonePersistentState(s PersistentState) PersistentState {
	raw, _ := json.Marshal(s)
	var out PersistentState
	_ = json.Unmarshal(raw, &out)
	if out.CredentialChains == nil {
		out.CredentialChains = map[AuthInstanceID]TransitionChain{}
	}
	return out
}

type RecoveryReport struct {
	Migrated       bool
	ReadOnly       bool
	UsedBackup     bool
	RecoveredEmpty bool
}
type FileHooks struct{ Observe func(string) }

func OSFileHooks() FileHooks { return FileHooks{} }
func (h FileHooks) event(s string) {
	if h.Observe != nil {
		h.Observe(s)
	}
}

type StateStore struct {
	mu       sync.Mutex
	path     string
	hooks    FileHooks
	crash    CrashHitter
	readOnly bool
}

func NewStateStore(path string, hooks FileHooks, crash CrashHitter) *StateStore {
	return &StateStore{path: path, hooks: hooks, crash: crash}
}
func (s *StateStore) hit(name string) error {
	if s.crash != nil {
		return s.crash.Hit(name)
	}
	return nil
}
func (s *StateStore) Load() (PersistentState, RecoveryReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := readPersistentState(s.path)
	report := RecoveryReport{}
	if err != nil {
		state, err = readPersistentState(s.path + ".bak")
		if err == nil {
			report.UsedBackup = true
		} else {
			state = NewPersistentState()
			report.RecoveredEmpty = true
			return state, report, nil
		}
	}
	if state.SchemaVersion > CurrentStateSchema {
		s.readOnly = true
		report.ReadOnly = true
		return state, report, nil
	}
	if state.SchemaVersion < CurrentStateSchema {
		state.SchemaVersion = CurrentStateSchema
		report.Migrated = true
	}
	if state.CredentialChains == nil {
		state.CredentialChains = map[AuthInstanceID]TransitionChain{}
	}
	return state, report, nil
}
func readPersistentState(path string) (PersistentState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return PersistentState{}, err
	}
	var st PersistentState
	if err = json.Unmarshal(raw, &st); err != nil {
		return PersistentState{}, err
	}
	return st, nil
}
func (s *StateStore) WriteThrough(state PersistentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readOnly {
		return ErrStateReadOnly
	}
	state.SchemaVersion = CurrentStateSchema
	if state.CredentialChains == nil {
		state.CredentialChains = map[AuthInstanceID]TransitionChain{}
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(s.path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	s.hooks.event("temp-write")
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	s.hooks.event("temp-fsync")
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = s.hit("K_STATE_BEFORE_RENAME"); err != nil {
		return err
	}
	if current, readErr := os.ReadFile(s.path); readErr == nil {
		_ = os.WriteFile(s.path+".bak", current, 0600)
	}
	//kpoint:K_STATE_RENAME_BEFORE
	s.hooks.event("rename-before")
	if err = os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.hooks.event("rename-after")
	//kpoint:K_STATE_RENAME_AFTER
	if _, err = os.Stat(s.path + ".bak"); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(s.path+".bak", raw, 0600)
	}
	s.hooks.event("dir-fsync")
	if d, openErr := os.Open(dir); openErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return s.hit("K_STATE_AFTER_RENAME")
}
