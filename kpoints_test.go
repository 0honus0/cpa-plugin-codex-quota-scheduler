package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestS2KPointRegistryMatchesSource(t *testing.T) {
	expectedRaw, err := os.ReadFile("scripts/refactor_gates/s2-kpoints.txt")
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Fields(string(expectedRaw))
	re := regexp.MustCompile(`//kpoint:(K_[A-Z0-9_]+)`)
	found := map[string]bool{}
	for _, name := range []string{"credential_wal.go", "state_store.go", "fence.go"} {
		raw, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllSubmatch(raw, -1) {
			found[string(m[1])] = true
		}
	}
	var actual []string
	for k := range found {
		actual = append(actual, k)
	}
	sort.Strings(actual)
	sort.Strings(expected)
	if strings.Join(actual, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("k-points actual=%v expected=%v", actual, expected)
	}
}
