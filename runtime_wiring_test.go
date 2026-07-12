package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeffery/codex-quota-scheduler/testsupport"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRuntimeSemanticPathsAndLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.json")
	paths := semanticStatePaths(legacy)
	if paths.UserData != filepath.Join(dir, ".user-data.json") || paths.Runtime != filepath.Join(dir, ".runtime-state.json") {
		t.Fatalf("paths = %#v", paths)
	}
	want := PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "legacy"}}}
	if err := SavePluginDiskState(legacy, want); err != nil {
		t.Fatal(err)
	}
	got, loaded, err := loadUserDataWithMigration(paths, OSFileHooks(), nil)
	if err != nil || !loaded || got.Accounts["a"].Alias != "legacy" {
		t.Fatalf("got=%#v loaded=%v err=%v", got, loaded, err)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatalf("legacy not retained: %v", err)
	}
	raw, err := os.ReadFile(paths.UserData)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.SchemaVersion != CurrentUserDataSchema {
		t.Fatalf("new user data lacks schema: %s", raw)
	}

	if err := os.WriteFile(legacy, []byte(`{"accounts":{"a":{"alias":"ignored"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, _, err = loadUserDataWithMigration(paths, OSFileHooks(), nil)
	if err != nil || got.Accounts["a"].Alias != "legacy" {
		t.Fatalf("new file not authoritative: %#v %v", got, err)
	}
}

func TestRuntimeCorruptLegacyIsPreserved(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.json")
	if err := os.WriteFile(legacy, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadUserDataWithMigration(semanticStatePaths(legacy), OSFileHooks(), nil)
	if err == nil {
		t.Fatal("expected corrupt legacy error")
	}
	if raw, readErr := os.ReadFile(legacy); readErr != nil || string(raw) != "{bad" {
		t.Fatalf("legacy changed: %q %v", raw, readErr)
	}
	if _, statErr := os.Stat(legacy + ".migrated"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected migrated file: %v", statErr)
	}
}

func TestUserDataRecoversFromAtomicBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".user-data.json")
	if err := SaveUserData(path, PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "first"}}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveUserData(path, PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "second"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	got, loaded, err := loadUserData(path)
	if err != nil || !loaded || got.Accounts["a"].Alias != "first" {
		t.Fatalf("backup recovery got=%#v loaded=%v err=%v", got, loaded, err)
	}
}

func TestUserDataMigrationCrashPointsConverge(t *testing.T) {
	points := []string{"K_USER_MIGRATION_BEFORE_NEW_RENAME", "K_USER_MIGRATION_AFTER_NEW_RENAME", "K_USER_MIGRATION_BEFORE_LEGACY_RENAME", "K_USER_MIGRATION_AFTER_LEGACY_RENAME"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, "state.json")
			paths := semanticStatePaths(legacy)
			if err := SavePluginDiskState(legacy, PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "safe"}}}); err != nil {
				t.Fatal(err)
			}
			registry := testsupport.NewKPointRegistry(points...)
			crash := testsupport.NewCrashController(registry, point)
			if _, _, err := loadUserDataWithMigration(paths, OSFileHooks(), crash); !errors.Is(err, testsupport.ErrInjectedCrash) {
				t.Fatalf("crash err=%v", err)
			}
			if _, err := os.Stat(paths.Legacy); err != nil {
				if _, migratedErr := os.Stat(paths.Legacy + ".migrated"); migratedErr != nil {
					t.Fatalf("neither old evidence exists: %v / %v", err, migratedErr)
				}
			}
			got, loaded, err := loadUserDataWithMigration(paths, OSFileHooks(), nil)
			if err != nil || !loaded || got.Accounts["a"].Alias != "safe" {
				t.Fatalf("recovery got=%#v loaded=%v err=%v", got, loaded, err)
			}
		})
	}
}

func TestRuntimeAndUserArtifactsNeverPersistSensitiveValues(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.json")
	paths := semanticStatePaths(legacy)
	if err := SavePluginDiskState(legacy, PluginDiskState{Config: DefaultConfig()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadUserDataWithMigration(paths, OSFileHooks(), nil); err != nil {
		t.Fatal(err)
	}
	store := NewStateStore(paths.Runtime, OSFileHooks(), nil)
	if _, err := store.Update(func(s *PersistentState) error {
		s.CredentialChains[1] = TransitionChain{Cursor: NewCredentialFingerprint("subject-secret", "refresh-token-secret", "Authorization: Bearer header-secret")}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".bak", ".tmp", ".corrupt"} {
		for _, base := range []string{paths.Runtime, paths.UserData} {
			raw, err := os.ReadFile(base + suffix)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"subject-secret", "refresh-token-secret", "header-secret"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("secret %q in %s", secret, base+suffix)
				}
			}
		}
	}
}

func TestProbeAttemptSeamRuntimeRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runtime-state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	sent := time.Unix(11, 0).UTC()
	want := ProbeAttemptSeam{Instance: 7, AttemptID: "attempt-1", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSentUnknown, SendFenceSeq: 9, CreatedAt: time.Unix(10, 0).UTC(), SentAt: &sent, VerifyNotBefore: time.Unix(20, 0).UTC(), SuppressUntil: time.Unix(30, 0).UTC()}
	if _, err := store.Update(func(s *PersistentState) error { s.ProbeAttempts[want.Instance] = want; return nil }); err != nil {
		t.Fatal(err)
	}
	fresh := NewStateStore(path, OSFileHooks(), nil)
	got, _, err := fresh.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.ProbeAttempts[7].AttemptID != want.AttemptID || got.ProbeAttempts[7].Phase != want.Phase || got.ProbeAttempts[7].SentAt == nil {
		t.Fatalf("round trip = %#v", got.ProbeAttempts[7])
	}
}

type runtimeCredentialHost struct {
	current map[AuthInstanceID]HostAuth
	saves   int
	gets    int
}

func (h *runtimeCredentialHost) SaveAuth(_ context.Context, id AuthInstanceID, auth HostAuth) error {
	h.saves++
	h.current[id] = auth
	return nil
}
func (h *runtimeCredentialHost) GetAuth(_ context.Context, id AuthInstanceID) (HostAuth, error) {
	h.gets++
	a, ok := h.current[id]
	if !ok {
		return HostAuth{}, os.ErrNotExist
	}
	return a, nil
}

func TestBindingGenesisAndInjectedCredentialWAL(t *testing.T) { //inv:INV-33
	store := NewStateStore(filepath.Join(t.TempDir(), ".runtime-state.json"), OSFileHooks(), nil)
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}
	registry, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "auth-a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(3)}}}
	observed := HostAuth{Fingerprint: NewCredentialFingerprint("subject", "refresh", `{"account":"a"}`)}
	host.current[legacyAuthInstanceID("auth-a")] = observed
	binding, created, err := registry.ObserveAuthoritative(context.Background(), roster, "auth-a", host)
	if err != nil || !created {
		t.Fatalf("binding=%#v created=%v err=%v", binding, created, err)
	}
	if binding.Login != 1 || binding.Token != 1 || binding.Admission != 1 || binding.Generation != 1 || binding.Fingerprint != observed.Fingerprint {
		t.Fatalf("bad genesis: %#v", binding)
	}
	if host.gets != 1 || host.saves != 0 {
		t.Fatalf("genesis calls get=%d save=%d", host.gets, host.saves)
	}
	token := binding.ExecutionToken(41)
	manager, err := NewCredentialManager(store, host, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	next := HostAuth{Fingerprint: NewCredentialFingerprint("subject", "refresh-2", `{"account":"a"}`)}
	if _, err := manager.SaveVersioned(context.Background(), binding.Instance, next, token); err != nil {
		t.Fatal(err)
	}
	if host.saves != 1 {
		t.Fatalf("save calls=%d", host.saves)
	}
	fresh, err := NewBindingRegistry(NewStateStore(store.path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh.Lookup("auth-a"); !ok {
		t.Fatal("binding not persistent across restart")
	}
}

func TestBindingGenesisFailsClosedForCapabilityBAndAuthBlocked(t *testing.T) { //inv:INV-33
	store := NewStateStore(filepath.Join(t.TempDir(), ".runtime-state.json"), OSFileHooks(), nil)
	host := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}
	registry, err := NewBindingRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.ObserveAuthoritative(context.Background(), HostRosterSnapshot{Capability: CapabilityB}, "auth-a", host); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("err=%v", err)
	}
	if host.gets != 0 || host.saves != 0 {
		t.Fatalf("Capability-B made calls: get=%d save=%d", host.gets, host.saves)
	}
	registry.MarkAuthBlocked("auth-a")
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "auth-a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(1)}}}
	host.current[legacyAuthInstanceID("auth-a")] = HostAuth{Fingerprint: fp("s", "r", "m")}
	binding, _, err := registry.ObserveAuthoritative(context.Background(), roster, "auth-a", host)
	if err != nil {
		t.Fatal(err)
	}
	if !binding.AuthBlocked {
		t.Fatal("genesis unlocked AuthBlocked")
	}
	if err := os.Remove(store.path); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewBindingRegistry(NewStateStore(store.path, OSFileHooks(), nil))
	if err != nil {
		t.Fatal(err)
	}
	recovered, ok := fresh.Lookup("auth-a")
	if !ok || !recovered.AuthBlocked {
		t.Fatalf("primary deletion bypassed AuthBlocked: %#v ok=%v", recovered, ok)
	}
}

func TestQuotaRefresherProductionDependenciesAreInjected(t *testing.T) {
	dir := t.TempDir()
	credentialHost := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}
	r, err := NewProductionQuotaRefresher(&fakeHostClient{}, NewPluginState(DefaultConfig()), credentialHost, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(dir, "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if r.runtimeStore == nil || r.bindings == nil || r.credentials == nil {
		t.Fatalf("runtime dependencies missing: %#v", r)
	}
	if _, _, err := r.BootstrapBinding(context.Background(), "candidate-only"); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("err=%v", err)
	}
	if credentialHost.gets != 0 || credentialHost.saves != 0 {
		t.Fatalf("Capability-B external calls get=%d save=%d", credentialHost.gets, credentialHost.saves)
	}
}

func TestProductionRosterAdapterGenesisAndWALSmoke(t *testing.T) {
	host := &fakeHostClient{authList: []pluginapi.HostAuthFileEntry{{ID: "auth-a", AuthIndex: "idx-a", Name: "codex-a.json", Provider: "codex", Priority: 3}}, authJSON: map[string]json.RawMessage{"idx-a": json.RawMessage(`{"access_token":"access","refresh_token":"refresh","account_id":"acct"}`)}}
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "auth-a", AuthIndex: "idx-a", Provider: "codex", Priority: intPtr(3)}}}
	adapter := &rosterCredentialHost{host: host, roster: roster}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, roster, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	binding, created, err := r.BootstrapBinding(context.Background(), "auth-a")
	if err != nil || !created {
		t.Fatalf("binding=%#v created=%v err=%v", binding, created, err)
	}
	nextRaw := []byte(`{"access_token":"access-2","refresh_token":"refresh-2","account_id":"acct"}`)
	next := HostAuth{Name: "codex-a.json", Raw: nextRaw, Fingerprint: NewCredentialFingerprint("acct", "refresh-2", "idx-a")}
	if _, err := r.credentials.SaveVersioned(context.Background(), binding.Instance, next, binding.ExecutionToken(1)); err != nil {
		t.Fatal(err)
	}
	if string(host.saved["codex-a.json"]) != string(nextRaw) || host.httpCalls != 0 {
		t.Fatalf("saved=%s httpCalls=%d", host.saved["codex-a.json"], host.httpCalls)
	}
}

func TestRuntimeConstructionFailureAuthorizesZeroExternalCalls(t *testing.T) {
	credentialHost := &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}
	badPath := filepath.Join(t.TempDir()+string(rune(0)), "state.json")
	r, err := NewProductionQuotaRefresher(&fakeHostClient{}, NewPluginState(DefaultConfig()), credentialHost, HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "idx", Provider: "codex", Priority: intPtr(1)}}}, badPath, time.Now)
	if err == nil || r != nil {
		t.Fatalf("runtime=%#v err=%v", r, err)
	}
	if credentialHost.gets != 0 || credentialHost.saves != 0 {
		t.Fatalf("construction failure made calls get=%d save=%d", credentialHost.gets, credentialHost.saves)
	}
}

func intPtr(v int) *int { return &v }

func TestSuiteRuntimeWiring(t *testing.T) {
	t.Run("semantic migration", TestRuntimeSemanticPathsAndLegacyMigration)
	t.Run("corrupt legacy", TestRuntimeCorruptLegacyIsPreserved)
	t.Run("user backup", TestUserDataRecoversFromAtomicBackup)
	t.Run("migration crashes", TestUserDataMigrationCrashPointsConverge)
	t.Run("sensitive scan", TestRuntimeAndUserArtifactsNeverPersistSensitiveValues)
	t.Run("probe seam", TestProbeAttemptSeamRuntimeRoundTrip)
	t.Run("genesis wal", TestBindingGenesisAndInjectedCredentialWAL)
	t.Run("fail closed", TestBindingGenesisFailsClosedForCapabilityBAndAuthBlocked)
	t.Run("production injection", TestQuotaRefresherProductionDependenciesAreInjected)
	t.Run("adapter wal smoke", TestProductionRosterAdapterGenesisAndWALSmoke)
	t.Run("construction failure", TestRuntimeConstructionFailureAuthorizesZeroExternalCalls)
}
