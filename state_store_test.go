package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStateStoreAtomicOrderingAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var events []string
	hooks := OSFileHooks()
	hooks.Observe = func(s string) { events = append(events, s) }
	store := NewStateStore(path, hooks, nil)
	state := NewPersistentState()
	state.ReservedCeiling = 42
	if err := store.WriteThrough(state); err != nil {
		t.Fatal(err)
	}
	want := []string{"temp-write", "temp-fsync", "rename-before", "rename-after", "dir-fsync"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v", events)
	}
	got, report, err := store.Load()
	if err != nil || report.ReadOnly || got.ReservedCeiling != 42 {
		t.Fatalf("got=%+v report=%+v err=%v", got, report, err)
	}
}

func TestStateStoreFutureVersionReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw, _ := json.Marshal(PersistentState{SchemaVersion: 99})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStateStore(path, OSFileHooks(), nil)
	_, report, err := store.Load()
	if err != nil || !report.ReadOnly {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if err := store.WriteThrough(NewPersistentState()); !errors.Is(err, ErrStateReadOnly) {
		t.Fatalf("err=%v", err)
	}
}

func TestStateStoreBackupAndDualCorruptionRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	s := NewPersistentState()
	s.ReservedCeiling = 7
	if err := store.WriteThrough(s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	got, report, err := store.Load()
	if err != nil || !report.UsedBackup || got.ReservedCeiling != 7 {
		t.Fatalf("got=%+v report=%+v err=%v", got, report, err)
	}
	os.WriteFile(path+".bak", []byte("bad"), 0600)
	got, report, err = store.Load()
	if err != nil || !report.RecoveredEmpty || got.SchemaVersion != CurrentStateSchema {
		t.Fatalf("got=%+v report=%+v err=%v", got, report, err)
	}
}

func TestStateStoreContainsNoSensitiveTerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewPersistentState()
	s.CredentialChains[1] = TransitionChain{Cursor: NewCredentialFingerprint("subject-raw", "refresh-token-raw", "authorization: bearer secret")}
	if err := NewStateStore(path, OSFileHooks(), nil).WriteThrough(s); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	lower := bytes.ToLower(raw)
	for _, term := range []string{"subject-raw", "refresh-token-raw", "authorization", "bearer secret", "cookie"} {
		if strings.Contains(string(lower), term) {
			t.Fatalf("state contains %q: %s", term, raw)
		}
	}
}

func TestStateStoreMigrationFromLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(path, []byte(`{"schema_version":0,"reserved_ceiling":9}`), 0600)
	got, report, err := NewStateStore(path, OSFileHooks(), nil).Load()
	if err != nil || !report.Migrated || got.SchemaVersion != CurrentStateSchema || got.ReservedCeiling != 9 {
		t.Fatalf("got=%+v report=%+v err=%v", got, report, err)
	}
}
