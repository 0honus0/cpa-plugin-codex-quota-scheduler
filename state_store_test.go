package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeffery/codex-quota-scheduler/testsupport"
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
	want := []string{"backup-write", "backup-fsync", "backup-dir-fsync", "primary-write", "primary-fsync", "rename-before", "rename-after", "primary-dir-fsync"}
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
	for _, candidate := range []string{path, path + ".bak", path + ".tmp", path + ".bak.tmp"} {
		raw, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		lower := bytes.ToLower(raw)
		for _, term := range []string{"subject-raw", "refresh-token-raw", "authorization", "bearer secret", "cookie"} {
			if strings.Contains(string(lower), term) {
				t.Fatalf("%s contains %q: %s", candidate, term, raw)
			}
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

func TestStateStoreRepeatedReplacementAndInjectedDurabilityFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	hooks := OSFileHooks()
	store := NewStateStore(path, hooks, nil)
	for _, v := range []uint64{1, 2, 3} {
		s := NewPersistentState()
		s.ReservedCeiling = v
		if err := store.WriteThrough(s); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := store.Load()
	if got.ReservedCeiling != 3 {
		t.Fatalf("ceiling=%d", got.ReservedCeiling)
	}
	hooks = OSFileHooks()
	hooks.Fail = func(op string) error {
		if op == "backup-fsync" {
			return errors.New("fsync")
		}
		return nil
	}
	store = NewStateStore(path, hooks, nil)
	s := NewPersistentState()
	s.ReservedCeiling = 4
	if err := store.WriteThrough(s); err == nil {
		t.Fatal("backup fsync failure ignored")
	}
}

func TestDualCorruptionMarksFenceUnsafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(path, []byte("bad"), 0600)
	os.WriteFile(path+".bak", []byte("bad"), 0600)
	store := NewStateStore(path, OSFileHooks(), nil)
	state, report, err := store.Load()
	if err != nil || !report.FenceUnsafe {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := NewFenceAllocator(store, state, nil).Next(); !errors.Is(err, ErrFenceUnsafe) {
		t.Fatalf("err=%v", err)
	}
}
func TestFenceQuarantineSurvivesCredentialUpdateAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(path, []byte("bad"), 0600)
	os.WriteFile(path+".bak", []byte("bad"), 0600)
	store := NewStateStore(path, OSFileHooks(), nil)
	state, _, _ := store.Load()
	if err := store.Update(func(s *PersistentState) error { s.NextSaveSeq = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	restart := NewStateStore(path, OSFileHooks(), nil)
	state, report, err := restart.Load()
	if err != nil || !report.FenceUnsafe {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := NewFenceAllocator(restart, state, nil).Next(); !errors.Is(err, ErrFenceUnsafe) {
		t.Fatalf("err=%v", err)
	}
}

func TestStateStoreReadErrorPropagatesWithoutTouchingBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	backup := []byte(`{"schema_version":1,"reserved_ceiling":77}`)
	os.WriteFile(path+".bak", backup, 0600)
	hooks := OSFileHooks()
	hooks.ReadFile = func(name string) ([]byte, error) {
		if name == path {
			return nil, os.ErrPermission
		}
		return os.ReadFile(name)
	}
	store := NewStateStore(path, hooks, nil)
	if _, _, err := store.Load(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err=%v", err)
	}
	after, _ := os.ReadFile(path + ".bak")
	if !bytes.Equal(after, backup) {
		t.Fatal("backup changed")
	}
}

func TestFenceOverflowUsesAuthoritativeCurrentCeiling(t *testing.T) {
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	state, _, _ := store.Load()
	_ = store.Update(func(s *PersistentState) error { s.ReservedCeiling = ^uint64(0) - FenceBlockSize + 1; return nil })
	if _, err := NewFenceAllocator(store, state, nil).Next(); !errors.Is(err, ErrFenceOverflow) {
		t.Fatalf("err=%v", err)
	}
}
func TestMissingFreshStateAllowsInitialFenceReservation(t *testing.T) {
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	state, report, err := store.Load()
	if err != nil || report.FenceUnsafe {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := NewFenceAllocator(store, state, nil).Next(); err != nil {
		t.Fatal(err)
	}
}

func TestStateStoreTransactionalMutationsDoNotClobber(t *testing.T) {
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	if _, _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = store.Update(func(s *PersistentState) error { s.ReservedCeiling = FenceBlockSize; return nil })
	}()
	go func() {
		defer wg.Done()
		_ = store.Update(func(s *PersistentState) error {
			s.NextSaveSeq = 9
			s.CredentialChains[1] = TransitionChain{Cursor: fp("s", "r", "m")}
			return nil
		})
	}()
	wg.Wait()
	got, _, err := store.Load()
	if err != nil || got.ReservedCeiling != FenceBlockSize || got.NextSaveSeq != 9 || len(got.CredentialChains) != 1 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestCredentialAndFenceConcurrentMutationsPreserveBoth(t *testing.T) {
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	state, _, _ := store.Load()
	prev := HostAuth{Fingerprint: fp("s", "0", "m")}
	manager := mustCredentialManager(t, store, &walHost{current: prev}, time.Now, nil)
	manager.SetChain(1, TransitionChain{Cursor: prev.Fingerprint})
	fence := NewFenceAllocator(store, state, nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = manager.SaveVersioned(context.Background(), 1, HostAuth{Fingerprint: fp("s", "1", "m")}, ExecutionToken{Instance: 1})
	}()
	go func() { defer wg.Done(); _, _ = fence.Next() }()
	wg.Wait()
	got, _, err := store.Load()
	if err != nil || got.ReservedCeiling != FenceBlockSize || got.NextSaveSeq != 1 || len(got.CredentialChains[1].Transitions) != 1 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestStateStoreReplacementFailurePropagates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	hooks := OSFileHooks()
	hooks.Replace = func(source, target string) error { return errors.New("replace failed") }
	if err := NewStateStore(path, hooks, nil).WriteThrough(NewPersistentState()); err == nil {
		t.Fatal("replace error ignored")
	}
}

func TestStateStoreSharedControllerCrashPoints(t *testing.T) {
	for _, point := range []string{"K_STATE_BEFORE_RENAME", "K_STATE_AFTER_RENAME"} {
		t.Run(point, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			err := NewStateStore(path, OSFileHooks(), crashController(point)).WriteThrough(NewPersistentState())
			if !errors.Is(err, testsupport.ErrInjectedCrash) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
