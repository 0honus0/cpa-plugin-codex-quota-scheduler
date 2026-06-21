package main

import (
	"os"
	"path/filepath"
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
