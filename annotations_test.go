package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestResolveAnnotationKeyPrefersAuthIDThenChatGPTAccountID(t *testing.T) {
	acct := AccountState{AuthID: "auth-1", ChatGPTAccountID: "acct-1", Email: "a@example.com"}
	if got := ResolveAnnotationKey(acct); got != "auth:auth-1" {
		t.Fatalf("key = %q, want auth:auth-1", got)
	}
	acct.AuthID = ""
	if got := ResolveAnnotationKey(acct); got != "chatgpt:acct-1" {
		t.Fatalf("key = %q, want chatgpt:acct-1", got)
	}
	acct.ChatGPTAccountID = ""
	if got := ResolveAnnotationKey(acct); got != "email:a@example.com" {
		t.Fatalf("key = %q, want email:a@example.com", got)
	}
}

func TestAnnotationStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "annotations.json")
	state := AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Alias: "Team A 01", Notes: "shared", Tags: []string{"team-a"}, GroupID: "team-a"},
		},
		Groups: map[string]GroupAnnotation{
			"team-a": {Name: "Team A", Notes: "weekly pool", Tags: []string{"team"}, Color: "#2563eb"},
		},
	}
	if err := SaveAnnotations(path, state); err != nil {
		t.Fatalf("SaveAnnotations returned error: %v", err)
	}
	loaded, err := LoadAnnotations(path)
	if err != nil {
		t.Fatalf("LoadAnnotations returned error: %v", err)
	}
	if loaded.Accounts["auth:auth-1"].Alias != "Team A 01" {
		t.Fatalf("loaded annotation = %#v", loaded.Accounts["auth:auth-1"])
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if len(raw) == 0 || raw[0] != '{' {
		t.Fatalf("annotation file is not JSON object: %q", raw)
	}
}

func TestSaveAnnotationsTightensExistingFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "annotations.json")
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := SaveAnnotations(path, AnnotationState{}); err != nil {
		t.Fatalf("SaveAnnotations returned error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat returned error: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("mode = %v, want 0600", got)
	}
}

func TestLoadAnnotationsMissingFileReturnsNormalizedState(t *testing.T) {
	state, err := LoadAnnotations(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadAnnotations returned error: %v", err)
	}
	if state.Accounts == nil {
		t.Fatal("Accounts map is nil")
	}
	if state.Groups == nil {
		t.Fatal("Groups map is nil")
	}
}

func TestNormalizeAnnotationStateCleansTags(t *testing.T) {
	state := NormalizeAnnotationState(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:auth-1": {Tags: []string{" team-a ", "", "team-a", "shared"}},
		},
		Groups: map[string]GroupAnnotation{
			"team-a": {Tags: []string{" team ", "team", "weekly"}},
		},
	})
	if got, want := state.Accounts["auth:auth-1"].Tags, []string{"team-a", "shared"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("account tags = %#v, want %#v", got, want)
	}
	if got, want := state.Groups["team-a"].Tags, []string{"team", "weekly"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("group tags = %#v, want %#v", got, want)
	}
}
