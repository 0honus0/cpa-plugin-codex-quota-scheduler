package main

import (
	"testing"
	"time"
)

func TestParseCodexUsageWeeklyWindows(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "plan_type": "plus",
	  "rate_limit": {
	    "allowed": true,
	    "primary_window": {"used_percent": 20, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
	    "secondary_window": {"used_percent": 55, "limit_window_seconds": 604800, "reset_after_seconds": 86400}
	  },
	  "rate_limit_reset_credits": {"available_count": 2}
	}`)
	parsed, err := ParseCodexUsagePayload(raw, now)
	if err != nil {
		t.Fatalf("ParseCodexUsagePayload returned error: %v", err)
	}
	if parsed.Family != AccountFamilyWeekly {
		t.Fatalf("Family = %q, want weekly", parsed.Family)
	}
	if parsed.FiveHour == nil || parsed.LongWindow == nil {
		t.Fatalf("missing windows: %#v", parsed)
	}
	if !parsed.LongWindow.ResetAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("weekly reset = %s, want %s", parsed.LongWindow.ResetAt, now.Add(24*time.Hour))
	}
	if parsed.ResetCreditsAvailableCount == nil || *parsed.ResetCreditsAvailableCount != 2 {
		t.Fatalf("reset credits = %#v", parsed.ResetCreditsAvailableCount)
	}
}

func TestParseCodexUsageMonthlyWindow(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "rate_limit": {
	    "primary_window": {"used_percent": 10, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
	    "secondary_window": {"used_percent": 30, "limit_window_seconds": 2592000, "reset_after_seconds": 172800}
	  }
	}`)
	parsed, err := ParseCodexUsagePayload(raw, now)
	if err != nil {
		t.Fatalf("ParseCodexUsagePayload returned error: %v", err)
	}
	if parsed.Family != AccountFamilyMonthly {
		t.Fatalf("Family = %q, want monthly", parsed.Family)
	}
	if !parsed.LongWindow.ResetAt.Equal(now.Add(48 * time.Hour)) {
		t.Fatalf("monthly reset = %s, want %s", parsed.LongWindow.ResetAt, now.Add(48*time.Hour))
	}
}

func TestParseCodexUsageExhaustedFromAllowedFalse(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "rate_limit": {
	    "allowed": false,
	    "limit_reached": true,
	    "primary_window": {"used_percent": 100, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
	    "secondary_window": {"used_percent": 20, "limit_window_seconds": 604800, "reset_after_seconds": 86400}
	  }
	}`)
	parsed, err := ParseCodexUsagePayload(raw, now)
	if err != nil {
		t.Fatalf("ParseCodexUsagePayload returned error: %v", err)
	}
	if parsed.FiveHour == nil || !parsed.FiveHour.Exhausted {
		t.Fatalf("five hour window not exhausted: %#v", parsed.FiveHour)
	}
}
