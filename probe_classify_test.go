package main

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestProbeClassifierOrderedRules(t *testing.T) { //inv:INV-17,INV-19
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	prev := now.Add(-time.Hour)
	usage := 80.0
	cases := []struct {
		name string
		base ProbeBaseline
		snap QuotaSnapshot
		want ProbeClassificationKind
	}{
		{"backwards", ResetProbeBaseline(prev, usage, 5*time.Hour), QuotaSnapshot{Valid: true, ResetAt: ptrTime(prev.Add(-3 * time.Minute)), Usage: ptrFloat(usage)}, ProbeAnomaly},
		{"large known jump", ResetProbeBaseline(prev, usage, 5*time.Hour), QuotaSnapshot{Valid: true, ResetAt: ptrTime(prev.Add(11 * time.Hour)), Usage: ptrFloat(usage)}, ProbeAnomaly},
		{"new reset", ResetProbeBaseline(prev, usage, 0), QuotaSnapshot{Valid: true, ResetAt: ptrTime(prev.Add(time.Hour)), Usage: ptrFloat(usage)}, ProbeActivatedNew},
		{"not due", ResetProbeBaseline(now.Add(30*time.Second), usage, 0), QuotaSnapshot{Valid: true, ResetAt: ptrTime(now.Add(30 * time.Second)), Usage: ptrFloat(usage)}, ProbeNotDueYet},
		{"missing reset cleared", ResetProbeBaseline(prev, usage, 0), QuotaSnapshot{Valid: true, Usage: ptrFloat(0)}, ProbeActivatedInferred},
		{"same reset lazy", ResetProbeBaseline(prev, usage, 0), QuotaSnapshot{Valid: true, ResetAt: ptrTime(prev), Usage: ptrFloat(usage)}, ProbeStillLazy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyProbeWindow(tc.base, tc.snap, now).Kind; got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestUsageOnlyNeverEntersResetRules(t *testing.T) { //inv:INV-19
	now := time.Unix(1000, 0).UTC()
	base := UsageOnlyProbeBaseline(75, now.Add(30*time.Minute))
	got := ClassifyProbeWindow(base, QuotaSnapshot{Valid: true, Usage: ptrFloat(75)}, now)
	if got.Kind != ProbeNotDueYet || got.Baseline.Kind != ProbeBaselineUsageOnly {
		t.Fatalf("classification=%#v", got)
	}
}

func ptrTime(v time.Time) *time.Time { return &v }
func ptrFloat(v float64) *float64    { return &v }

type probeGoldenRow struct {
	Baseline string `json:"baseline"`
	Offset   int    `json:"offset"`
	Length   string `json:"length"`
	Usage    string `json:"usage"`
	Delay    string `json:"delay"`
	Expected string `json:"expected"`
}

func generateProbeGolden() []probeGoldenRow {
	baselines := []string{"none", "reset", "usage_only"}
	offsets := []int{0, 1, 2, 3, 4, 5, 6, 7}
	lengths := []string{"5h", "7d", "unknown"}
	usages := []string{"cleared", "refilled", "same", "decreased"}
	delays := []string{"before", "edge", "after"}
	rows := make([]probeGoldenRow, 0, 864)
	for _, b := range baselines {
		for _, o := range offsets {
			for _, l := range lengths {
				for _, u := range usages {
					for _, d := range delays {
						rows = append(rows, probeGoldenRow{b, o, l, u, d, goldenProbeOracle(b, o, u, d)})
					}
				}
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		ka, kb := a.Baseline+a.Length+a.Usage+a.Delay, b.Baseline+b.Length+b.Usage+b.Delay
		return ka < kb || (ka == kb && a.Offset < b.Offset)
	})
	return rows
}
func goldenProbeOracle(b string, o int, u, d string) string {
	if b == "none" {
		if o == 0 {
			return "usage_only"
		}
		return "reset"
	}
	if b == "usage_only" {
		if o > 0 {
			return "reset"
		}
		if u == "cleared" || u == "refilled" {
			return "activated_inferred"
		}
		return "not_due_yet"
	}
	if o == 1 || o == 7 {
		return "anomaly"
	}
	if o >= 4 {
		return "activated_new"
	}
	if d == "before" {
		return "not_due_yet"
	}
	if u == "cleared" || u == "refilled" {
		return "activated_inferred"
	}
	return "still_lazy"
}
func TestProbeClassifyGolden(t *testing.T) { //inv:INV-17,INV-19
	want := generateProbeGolden()
	if len(want) != 864 {
		t.Fatalf("rows=%d", len(want))
	}
	if os.Getenv("UPDATE_PROBE_GOLDEN") == "1" {
		raw, _ := json.MarshalIndent(want, "", "  ")
		if err := os.WriteFile("testdata/probe_classify_golden.json", append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("testdata/probe_classify_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var got []probeGoldenRow
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("golden diff; regenerate testdata/probe_classify_golden.json")
	}
}
