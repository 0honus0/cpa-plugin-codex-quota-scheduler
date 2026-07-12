package refactorgate

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyzeDiscoversTransitiveHelperAndAliasedImports(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "entry.go", `package fixture
func handleSchedulerPick() { helper() }
`)
	writeFixture(t, root, "helper.go", `package fixture
import h "net/http"
import fs "os"
func helper() { _, _ = fs.ReadFile("x"); _ = h.MethodGet }
`)
	got, err := Analyze(root, "handleSchedulerPick")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"disk|os.ReadFile": false, "network|net/http.MethodGet": false}
	for _, violation := range got {
		key := violation.Type + "|" + violation.Symbol
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("missing %s in %#v", key, got)
		}
	}
}

func TestAnalyzeCountsStableSymbols(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "entry.go", `package fixture
import fs "os"
func handleSchedulerPick() { _, _ = fs.ReadFile("a"); _, _ = fs.ReadFile("b") }
`)
	got, err := Analyze(root, "handleSchedulerPick")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Symbol != "os.ReadFile" || got[0].Count != 2 {
		t.Fatalf("violations = %#v", got)
	}
}
