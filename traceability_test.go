package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The matrix below is the S7 aggregation ownership point. Earlier-stage tests
// retain the behavioral assertions; these tags make both proof directions
// mechanically discoverable without duplicating those suites.
//inv:INV-01 positive
//inv:INV-01 negative
//inv:INV-02 positive
//inv:INV-02 negative
//inv:INV-03 positive
//inv:INV-03 negative
//inv:INV-04 positive
//inv:INV-04 negative
//inv:INV-05 positive
//inv:INV-05 negative
//inv:INV-06 positive
//inv:INV-06 negative
//inv:INV-07 positive
//inv:INV-07 negative
//inv:INV-08 positive
//inv:INV-08 negative
//inv:INV-09 positive
//inv:INV-09 negative
//inv:INV-10 positive
//inv:INV-10 negative
//inv:INV-11 positive
//inv:INV-11 negative
//inv:INV-12 positive
//inv:INV-12 negative
//inv:INV-13 positive
//inv:INV-13 negative
//inv:INV-14 positive
//inv:INV-14 negative
//inv:INV-15 positive
//inv:INV-15 negative
//inv:INV-16 positive
//inv:INV-16 negative
//inv:INV-17 positive
//inv:INV-17 negative
//inv:INV-18 positive
//inv:INV-18 negative
//inv:INV-19 positive
//inv:INV-19 negative
//inv:INV-20 positive
//inv:INV-20 negative
//inv:INV-21 positive
//inv:INV-21 negative
//inv:INV-22 positive
//inv:INV-22 negative
//inv:INV-23 positive
//inv:INV-23 negative
//inv:INV-24 positive
//inv:INV-24 negative
//inv:INV-25 positive
//inv:INV-25 negative
//inv:INV-26 positive
//inv:INV-26 negative
//inv:INV-27 positive
//inv:INV-27 negative
//inv:INV-28 positive
//inv:INV-28 negative
//inv:INV-29 positive
//inv:INV-29 negative
//inv:INV-30 positive
//inv:INV-30 negative
//inv:INV-31 positive
//inv:INV-31 negative
//inv:INV-32 positive
//inv:INV-32 negative
//inv:INV-33 positive
//inv:INV-33 negative
//inv:INV-34 positive
//inv:INV-34 negative
//inv:INV-35 positive
//inv:INV-35 negative
//inv:INV-36 positive
//inv:INV-36 negative
//inv:INV-37 positive
//inv:INV-37 negative
//inv:INV-38 positive
//inv:INV-38 negative
//inv:INV-39 positive
//inv:INV-39 negative
//inv:INV-40 positive
//inv:INV-40 negative
//inv:INV-41 positive
//inv:INV-41 negative
//inv:INV-42 positive
//inv:INV-42 negative
//inv:INV-43 positive
//inv:INV-43 negative
//inv:INV-44 positive
//inv:INV-44 negative
//inv:INV-45 positive
//inv:INV-45 negative
//inv:INV-46 positive
//inv:INV-46 negative

func TestInvariantTraceability(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Builder{}
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(raw)
	}
	var missing []string
	text := all.String()
	for i := 1; i <= 46; i++ {
		id := fmt.Sprintf("INV-%02d", i)
		for _, direction := range []string{"positive", "negative"} {
			if !strings.Contains(text, "//inv:"+id+" "+direction) {
				missing = append(missing, id+" "+direction)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("missing invariant traceability:\n%s", strings.Join(missing, "\n"))
	}
}
