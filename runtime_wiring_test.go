package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

func TestLegacyMigrationRejectsSensitiveAndUnknownFieldsWithoutRename(t *testing.T) {
	for name, raw := range map[string]string{
		"refresh_token": `{"config":{},"refresh_token":"secret"}`,
		"authorization": `{"config":{},"Authorization":"Bearer secret"}`,
		"unknown":       `{"config":{},"credential_blob":{"token":"secret"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, "state.json")
			if err := os.WriteFile(legacy, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			_, _, err := loadUserDataWithMigration(semanticStatePaths(legacy), OSFileHooks(), nil)
			if err == nil {
				t.Fatal("expected strict migration rejection")
			}
			got, e := os.ReadFile(legacy)
			if e != nil || string(got) != raw {
				t.Fatalf("legacy=%q err=%v", got, e)
			}
			if _, e := os.Stat(legacy + ".migrated"); !errors.Is(e, os.ErrNotExist) {
				t.Fatalf("unexpected migrated evidence: %v", e)
			}
		})
	}
}

func TestLegacyMigrationRejectsCredentialSyntaxInsideAllowedStringValues(t *testing.T) {
	cases := map[string]string{
		"alias authorization":        `{"config":{},"accounts":{"a":{"alias":"Authorization: Bearer SECRET_X"}}}`,
		"group notes cookie":         `{"config":{},"groups":{"g":{"notes":"Cookie: session=SECRET_X"}}}`,
		"tag refresh assignment":     `{"config":{},"accounts":{"a":{"tags":["refresh_token=SECRET_X"]}}}`,
		"config credential material": `{"config":{"monthly_mode":"Authorization: Bearer SECRET_X"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, "state.json")
			if err := os.WriteFile(legacy, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			paths := semanticStatePaths(legacy)
			if _, _, err := loadUserDataWithMigration(paths, OSFileHooks(), nil); err == nil {
				t.Fatal("expected credential-value rejection")
			}
			got, e := os.ReadFile(legacy)
			if e != nil || string(got) != raw {
				t.Fatalf("legacy=%q err=%v", got, e)
			}
			for _, path := range []string{legacy + ".migrated", paths.UserData} {
				if _, e := os.Stat(path); !errors.Is(e, os.ErrNotExist) {
					t.Fatalf("unexpected artifact %s: %v", path, e)
				}
			}
		})
	}
}

func TestLegacyMigrationAllowsBenignTokenMentionAndScansRealArtifact(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "state.json")
	state := PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "token budgeting notes", Notes: "No credential syntax here", Tags: []string{"token-awareness"}}}}
	if err := SavePluginDiskState(legacy, state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadUserDataWithMigration(semanticStatePaths(legacy), OSFileHooks(), nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(legacy + ".migrated")
	if err != nil {
		t.Fatal(err)
	}
	if containsSensitiveCredentialMaterial(string(raw)) {
		t.Fatalf("real migrated artifact contains credential syntax: %s", raw)
	}
}

func TestValidLegacyMigrationScansActualRetainedArtifact(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "custom-legacy.json")
	state := PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "safe"}}}
	if err := SavePluginDiskState(legacy, state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadUserDataWithMigration(semanticStatePaths(legacy), OSFileHooks(), nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(legacy + ".migrated")
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(raw))
	if containsSensitiveCredentialMaterial(lower) {
		t.Fatalf("credential syntax in actual migrated artifact: %s", raw)
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
	points := []string{"K_USER_MIGRATION_BEFORE_NEW_RENAME", "K_USER_MIGRATION_AFTER_NEW_RENAME", "K_USER_MIGRATION_BEFORE_LEGACY_RENAME", "K_USER_MIGRATION_AFTER_LEGACY_RENAME", "K_USER_MIGRATION_AFTER_READBACK", "K_USER_PRIMARY_TEMP_WRITE", "K_USER_PRIMARY_TEMP_FSYNC", "K_USER_BACKUP_TEMP_WRITE", "K_USER_BACKUP_TEMP_FSYNC", "K_USER_BACKUP_BEFORE_REPLACE", "K_USER_BACKUP_AFTER_REPLACE", "K_USER_PRIMARY_DIR_FSYNC", "K_USER_AFTER_PRIMARY_DIR_FSYNC", "K_USER_BACKUP_DIR_FSYNC", "K_USER_AFTER_BACKUP_DIR_FSYNC"}
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
	for _, base := range []string{paths.Runtime, paths.UserData} {
		primary, err := os.ReadFile(base)
		if err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{".tmp", ".corrupt", ".migrated"} {
			if err := os.WriteFile(base+suffix, primary, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, suffix := range []string{"", ".bak", ".tmp", ".corrupt", ".migrated"} {
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
	if !reflect.DeepEqual(got.ProbeAttempts[7], want) {
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
	if err := registry.MarkAuthBlocked("auth-a"); err != nil {
		t.Fatal(err)
	}
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

func TestMarkAuthBlockedPropagatesFirstWriteAndBackupFailures(t *testing.T) {
	for _, op := range []string{"backup-write", "backup-fsync"} {
		t.Run(op, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".runtime-state.json")
			hooks := OSFileHooks()
			hooks.Fail = func(got string) error {
				if got == op {
					return errors.New("injected " + op)
				}
				return nil
			}
			registry, err := NewBindingRegistry(NewStateStore(path, hooks, nil))
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.MarkAuthBlocked("a"); err == nil {
				t.Fatal("expected persistence error")
			}
			if _, ok := registry.Lookup("a"); ok {
				t.Fatal("failed persistence mutated registry")
			}
		})
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

func TestProductionRefresherCapabilityBActualPathMakesZeroCalls(t *testing.T) {
	host := &countingProductionHost{}
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.Start()
	defer r.Stop()
	if err := r.RefreshOnce(); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("RefreshOnce err=%v", err)
	}
	r.RefreshSoon()
	r.RefreshDueSoon()
	r.RefreshOneSoon("candidate")
	r.RefreshDueCandidatesSoon(pluginapi.SchedulerPickRequest{}, 0)
	if host.total() != 0 {
		t.Fatalf("Capability-B production calls=%#v", host)
	}
}

func TestProductionRosterPublicationBootstrapsHighestTierAndUsesWAL(t *testing.T) {
	host := &countingProductionHost{httpResp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"access_token":"new","refresh_token":"r1","account_id":"acct"}`)}, auth: map[string]pluginapi.HostAuthGetResponse{
		"high": {AuthIndex: "high", Name: "high.json", JSON: json.RawMessage(`{"access_token":"old","refresh_token":"r0","account_id":"acct"}`)},
		"low":  {AuthIndex: "low", Name: "low.json", JSON: json.RawMessage(`{"access_token":"old","refresh_token":"r-low","account_id":"low"}`)},
	}}
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.Start()
	defer r.Stop()
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "low", AuthIndex: "low", Provider: "codex", Priority: intPtr(1)}, {ID: "high", AuthIndex: "high", Provider: "codex", Priority: intPtr(9)}}}
	if err := r.PublishAuthoritativeRoster(context.Background(), roster); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	running := r.running
	r.mu.Unlock()
	if !running {
		t.Fatal("A publication did not activate requested runtime")
	}
	if _, ok := r.bindings.Lookup("high"); !ok {
		t.Fatal("highest-tier binding missing")
	}
	if _, ok := r.bindings.Lookup("low"); ok {
		t.Fatal("lower-tier binding bootstrapped")
	}
	version := r.state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 9, AuthIDs: map[string]struct{}{"high": {}}})
	current := CodexCredentials{AccessToken: "old", RefreshToken: "r0", ChatGPTAccountID: "acct", ExpiresAt: time.Unix(1, 0)}
	if _, err := r.refreshAndSaveCredentials(host.auth["high"], current, "high", version); err != nil {
		t.Fatal(err)
	}
	if host.save != 1 || host.savedName != "high.json" {
		t.Fatalf("save=%d name=%q", host.save, host.savedName)
	}
}

type countingProductionHost struct {
	list, get, http, save, probe int
	auth                         map[string]pluginapi.HostAuthGetResponse
	savedName                    string
	httpResp                     pluginapi.HTTPResponse
	getErr                       map[string]error
	requests                     []pluginapi.HTTPRequest
}

func (h *countingProductionHost) total() int { return h.list + h.get + h.http + h.save + h.probe }
func (h *countingProductionHost) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	h.list++
	return nil, nil
}
func (h *countingProductionHost) GetAuth(i string) (pluginapi.HostAuthGetResponse, error) {
	h.get++
	if err := h.getErr[i]; err != nil {
		return pluginapi.HostAuthGetResponse{}, err
	}
	return h.auth[i], nil
}
func (h *countingProductionHost) SaveAuth(n string, _ json.RawMessage) error {
	h.save++
	h.savedName = n
	return nil
}
func (h *countingProductionHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.http++
	h.requests = append(h.requests, req)
	if req.URL == codexResetProbeEndpoint {
		h.probe++
	}
	return h.httpResp, nil
}

func TestProductionRosterLifecycleGatesBackgroundRequests(t *testing.T) {
	host := &countingProductionHost{httpResp: pluginapi.HTTPResponse{StatusCode: http.StatusOK}}
	state := NewPluginState(DefaultConfig())
	version := state.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 7, AuthIDs: map[string]struct{}{"a": {}}})
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, state, adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings

	healthy := ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterHealthy, BackgroundAllowed: true, Instances: []string{"a"}}
	r.ObserveRosterLifecycle(healthy)
	if !r.normalBackgroundAllowed() || !r.probeBackgroundAllowed() {
		t.Fatal("healthy roster did not authorize normal and probe background work")
	}

	degraded := healthy
	degraded.Health = RosterDegraded
	degraded.DegradedSince = time.Now().Add(-time.Minute)
	r.ObserveRosterLifecycle(degraded)
	if _, err := r.doWithAdmissionPermit("a", version, pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid/quota"}); err != nil {
		t.Fatal(err)
	}
	if len(host.requests) != 1 || host.requests[0].Headers.Get(rosterLifecycleRequestHeader) != rosterLifecycleDegraded {
		t.Fatalf("degraded request marker=%q requests=%#v", host.requests[0].Headers.Get(rosterLifecycleRequestHeader), host.requests)
	}

	failClosed := degraded
	failClosed.Health = RosterFailClosed
	failClosed.BackgroundAllowed = false
	r.ObserveRosterLifecycle(failClosed)
	if r.normalBackgroundAllowed() || r.probeBackgroundAllowed() {
		t.Fatal("fail-closed roster authorized background work")
	}
	if _, err := r.doWithAdmissionPermit("a", version, pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid/quota"}); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("fail-closed HTTP err=%v", err)
	}
	if len(host.requests) != 1 {
		t.Fatalf("fail-closed started HTTP: %#v", host.requests)
	}

	provisional := ActiveRoster{Capability: CapabilityB, Provisional: true, Health: RosterWaiting, BackgroundAllowed: true, Instances: []string{"a"}}
	r.ObserveRosterLifecycle(provisional)
	if r.normalBackgroundAllowed() || !r.probeBackgroundAllowed() {
		t.Fatalf("provisional gates normal=%v probe=%v", r.normalBackgroundAllowed(), r.probeBackgroundAllowed())
	}
}

func TestQuotaRefresherLifecycleCannotBeBypassedWithoutRuntimeStore(t *testing.T) {
	host := &countingProductionHost{httpResp: pluginapi.HTTPResponse{StatusCode: http.StatusOK}}
	r := NewQuotaRefresher(host, NewPluginState(DefaultConfig()), time.Now)
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed})
	for _, probe := range []bool{false, true} {
		if _, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid"}, probe); !errors.Is(err, ErrCapabilityB) {
			t.Fatalf("probe=%v err=%v", probe, err)
		}
	}
	if host.http != 0 {
		t.Fatalf("nil runtime store bypassed lifecycle: calls=%d", host.http)
	}
}

type lifecycleFenceHost struct {
	entered  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	requests []pluginapi.HTTPRequest
}

func (h *lifecycleFenceHost) ListAuths() ([]pluginapi.HostAuthFileEntry, error) { return nil, nil }
func (h *lifecycleFenceHost) GetAuth(string) (pluginapi.HostAuthGetResponse, error) {
	return pluginapi.HostAuthGetResponse{}, nil
}
func (h *lifecycleFenceHost) SaveAuth(string, json.RawMessage) error { return nil }
func (h *lifecycleFenceHost) Log(string, string, map[string]any)     {}
func (h *lifecycleFenceHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	h.mu.Unlock()
	select {
	case <-h.entered:
	default:
		close(h.entered)
	}
	<-h.release
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK}, nil
}

func TestProductionLifecyclePublicationFencesHTTPStart(t *testing.T) {
	host := &lifecycleFenceHost{entered: make(chan struct{}), release: make(chan struct{})}
	r := NewQuotaRefresher(host, NewPluginState(DefaultConfig()), time.Now)
	r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterDegraded, BackgroundAllowed: true})
	requestDone := make(chan error, 1)
	go func() {
		_, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid"}, false)
		requestDone <- err
	}()
	<-host.entered
	observeStarted := make(chan struct{})
	observeDone := make(chan struct{})
	go func() {
		close(observeStarted)
		r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed})
		close(observeDone)
	}()
	<-observeStarted
	select {
	case <-observeDone:
		t.Fatal("FailClosed published while an authorized HTTP start boundary was open")
	case <-time.After(50 * time.Millisecond):
	}
	close(host.release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	<-observeDone
	if _, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid"}, false); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("post-publication request err=%v", err)
	}
}

func TestProductionProbeLifecycleHostCallGates(t *testing.T) {
	host := &countingProductionHost{httpResp: pluginapi.HTTPResponse{StatusCode: http.StatusOK}}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), &runtimeCredentialHost{current: map[AuthInstanceID]HostAuth{}}, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	refresherMu.Lock()
	previous := globalRosterController
	globalRosterController = nil
	refresherMu.Unlock()
	t.Cleanup(func() { refresherMu.Lock(); globalRosterController = previous; refresherMu.Unlock() })

	t.Run("enqueue denial", func(t *testing.T) {
		r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed})
		if err := r.RunProbeDueOnce(context.Background()); !errors.Is(err, ErrCapabilityB) {
			t.Fatalf("err=%v", err)
		}
		if host.http != 0 {
			t.Fatalf("denied Probe made host HTTP calls=%d", host.http)
		}
	})

	t.Run("final start denial", func(t *testing.T) {
		r.rosterMu.Lock()
		started := make(chan error, 1)
		go func() {
			_, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid/probe"}, true)
			started <- err
		}()
		r.roster = hostRosterSnapshotFromActive(ActiveRoster{Capability: CapabilityA, Confirmed: true, Health: RosterFailClosed})
		r.rosterMu.Unlock()
		if err := <-started; !errors.Is(err, ErrCapabilityB) {
			t.Fatalf("err=%v", err)
		}
		if host.http != 0 {
			t.Fatalf("final-start denial made host HTTP calls=%d", host.http)
		}
	})

	t.Run("provisional marker", func(t *testing.T) {
		r.ObserveRosterLifecycle(ActiveRoster{Capability: CapabilityB, Provisional: true, Health: RosterWaiting, BackgroundAllowed: true})
		if _, err := r.doBackgroundHTTPRequest(pluginapi.HTTPRequest{Method: http.MethodGet, URL: "https://example.invalid/probe"}, true); err != nil {
			t.Fatal(err)
		}
		if got := host.requests[len(host.requests)-1].Headers.Get(rosterLifecycleRequestHeader); got != rosterLifecycleProvisional {
			t.Fatalf("marker=%q", got)
		}
	})
}
func (h *countingProductionHost) Log(string, string, map[string]any) {}

func TestStateStoreQuarantinesCorruptionAndRepairsFromBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runtime-state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	if _, err := store.Update(func(s *PersistentState) error { s.ReservedCeiling = 7; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(s *PersistentState) error { s.ReservedCeiling = 8; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad-primary"), 0600); err != nil {
		t.Fatal(err)
	}
	fresh := NewStateStore(path, OSFileHooks(), nil)
	got, report, err := fresh.Load()
	if err != nil || !report.UsedBackup || got.ReservedCeiling != 7 {
		t.Fatalf("got=%#v report=%#v err=%v", got, report, err)
	}
	if raw, err := os.ReadFile(path + ".corrupt"); err != nil || string(raw) != "{bad-primary" {
		t.Fatalf("primary evidence=%q err=%v", raw, err)
	}
	if _, _, err := NewStateStore(path, OSFileHooks(), nil).Load(); err != nil {
		t.Fatalf("repaired primary unreadable: %v", err)
	}
}

func TestRosterPublicationStaysFailClosedUntilAllGenesisSucceeds(t *testing.T) {
	host := &countingProductionHost{auth: map[string]pluginapi.HostAuthGetResponse{"a": {AuthIndex: "a", Name: "a.json", JSON: json.RawMessage(`{"access_token":"x","refresh_token":"ra","account_id":"a"}`)}, "b": {AuthIndex: "b", Name: "b.json", JSON: json.RawMessage(`{"access_token":"x","refresh_token":"rb","account_id":"b"}`)}}, getErr: map[string]error{"b": errors.New("bootstrap failed")}}
	adapter := &rosterCredentialHost{host: host, roster: HostRosterSnapshot{Capability: CapabilityB}}
	r, err := NewProductionQuotaRefresher(host, NewPluginState(DefaultConfig()), adapter, HostRosterSnapshot{Capability: CapabilityB}, filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	adapter.bindings = r.bindings
	r.Start()
	defer r.Stop()
	roster := HostRosterSnapshot{Capability: CapabilityA, Entries: []RosterEntry{{ID: "a", AuthIndex: "a", Provider: "codex", Priority: intPtr(9)}, {ID: "b", AuthIndex: "b", Provider: "codex", Priority: intPtr(9)}}}
	if err := r.PublishAuthoritativeRoster(context.Background(), roster); err == nil {
		t.Fatal("expected second genesis failure")
	}
	if r.runtimeRoster().Capability != CapabilityB {
		t.Fatal("partial genesis published Capability-A")
	}
	before := host.total()
	if err := r.RefreshOnce(); !errors.Is(err, ErrCapabilityB) {
		t.Fatalf("refresh err=%v", err)
	}
	r.RefreshSoon()
	r.RefreshDueSoon()
	r.RefreshOneSoon("a")
	r.RefreshDueCandidatesSoon(pluginapi.SchedulerPickRequest{}, 0)
	if host.total() != before {
		t.Fatalf("failed publication authorized calls before=%d after=%d", before, host.total())
	}
	delete(host.getErr, "b")
	if err := r.PublishAuthoritativeRoster(context.Background(), roster); err != nil {
		t.Fatal(err)
	}
	if r.runtimeRoster().Capability != CapabilityA {
		t.Fatal("successful retry did not publish A")
	}
}

func TestStateStoreDualCorruptionPreservesBothEvidenceAndFences(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runtime-state.json")
	os.WriteFile(path, []byte("{p"), 0600)
	os.WriteFile(path+".bak", []byte("{b"), 0600)
	got, report, err := NewStateStore(path, OSFileHooks(), nil).Load()
	if err != nil || !report.FenceUnsafe || !got.FenceUnsafe {
		t.Fatalf("got=%#v report=%#v err=%v", got, report, err)
	}
	for name, want := range map[string]string{path + ".corrupt": "{p", path + ".bak.corrupt": "{b"} {
		raw, e := os.ReadFile(name)
		if e != nil || string(raw) != want {
			t.Fatalf("%s=%q err=%v", name, raw, e)
		}
	}
}

func TestUserDataSchemaMigrationAndCorruptionEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".user-data.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":0,"config":{},"accounts":{"a":{"alias":"old"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, loaded, err := loadUserData(path)
	if err != nil || !loaded || got.Accounts["a"].Alias != "old" {
		t.Fatalf("got=%#v loaded=%v err=%v", got, loaded, err)
	}
	raw, _ := os.ReadFile(path)
	var env struct {
		SchemaVersion int `json:"schema_version"`
	}
	json.Unmarshal(raw, &env)
	if env.SchemaVersion != CurrentUserDataSchema {
		t.Fatalf("schema=%d raw=%s", env.SchemaVersion, raw)
	}
	if err := SaveUserData(path, PluginDiskState{Config: DefaultConfig(), Accounts: map[string]AccountAnnotation{"a": {Alias: "new"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad-user"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadUserData(path); err != nil {
		t.Fatal(err)
	}
	evidence, e := os.ReadFile(path + ".corrupt")
	if e != nil || string(evidence) != "{bad-user" {
		t.Fatalf("evidence=%q err=%v", evidence, e)
	}
}

func TestUserDataFutureSchemaFailsSafeAndDualCorruptionPreservesEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".user-data.json")
	future := []byte(`{"schema_version":99,"config":{}}`)
	if err := os.WriteFile(path, future, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadUserData(path); !errors.Is(err, ErrUserDataFutureSchema) {
		t.Fatalf("future err=%v", err)
	}
	raw, _ := os.ReadFile(path)
	if !reflect.DeepEqual(raw, future) {
		t.Fatal("future schema rewritten")
	}
	os.Remove(path)
	os.WriteFile(path, []byte("{p"), 0600)
	os.WriteFile(path+".bak", []byte("{b"), 0600)
	_, _, report, err := loadUserDataRecover(path)
	if err != nil || !report.RecoveredDefault {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	for name, want := range map[string]string{path + ".corrupt": "{p", path + ".bak.corrupt": "{b"} {
		got, e := os.ReadFile(name)
		if e != nil || string(got) != want {
			t.Fatalf("%s=%q err=%v", name, got, e)
		}
	}
}

func intPtr(v int) *int { return &v }

func TestSuiteRuntimeWiring(t *testing.T) {
	t.Run("semantic migration", TestRuntimeSemanticPathsAndLegacyMigration)
	t.Run("corrupt legacy", TestRuntimeCorruptLegacyIsPreserved)
	t.Run("strict legacy", TestLegacyMigrationRejectsSensitiveAndUnknownFieldsWithoutRename)
	t.Run("legacy value secrets", TestLegacyMigrationRejectsCredentialSyntaxInsideAllowedStringValues)
	t.Run("benign token text", TestLegacyMigrationAllowsBenignTokenMentionAndScansRealArtifact)
	t.Run("actual migrated scan", TestValidLegacyMigrationScansActualRetainedArtifact)
	t.Run("user backup", TestUserDataRecoversFromAtomicBackup)
	t.Run("migration crashes", TestUserDataMigrationCrashPointsConverge)
	t.Run("sensitive scan", TestRuntimeAndUserArtifactsNeverPersistSensitiveValues)
	t.Run("probe seam", TestProbeAttemptSeamRuntimeRoundTrip)
	t.Run("genesis wal", TestBindingGenesisAndInjectedCredentialWAL)
	t.Run("fail closed", TestBindingGenesisFailsClosedForCapabilityBAndAuthBlocked)
	t.Run("blocked errors", TestMarkAuthBlockedPropagatesFirstWriteAndBackupFailures)
	t.Run("production injection", TestQuotaRefresherProductionDependenciesAreInjected)
	t.Run("adapter wal smoke", TestProductionRosterAdapterGenesisAndWALSmoke)
	t.Run("construction failure", TestRuntimeConstructionFailureAuthorizesZeroExternalCalls)
	t.Run("actual B path", TestProductionRefresherCapabilityBActualPathMakesZeroCalls)
	t.Run("roster publication", TestProductionRosterPublicationBootstrapsHighestTierAndUsesWAL)
	t.Run("runtime corruption repair", TestStateStoreQuarantinesCorruptionAndRepairsFromBackup)
	t.Run("atomic roster publication", TestRosterPublicationStaysFailClosedUntilAllGenesisSucceeds)
	t.Run("runtime dual corruption", TestStateStoreDualCorruptionPreservesBothEvidenceAndFences)
	t.Run("user schema corruption", TestUserDataSchemaMigrationAndCorruptionEvidence)
	t.Run("user future dual corruption", TestUserDataFutureSchemaFailsSafeAndDualCorruptionPreservesEvidence)
}
