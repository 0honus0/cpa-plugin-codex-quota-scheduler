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

func TestParseCodexUsageResetCreditsExpiryList(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "rate_limit_reset_credits": {
	    "available_count": 2,
	    "credits": [
	      {"expires_at": "2026-07-01T00:00:00Z"},
	      {"expiresAt": "2026-07-02T00:00:00Z"}
	    ]
	  }
	}`)
	parsed, err := ParseCodexUsagePayload(raw, now)
	if err != nil {
		t.Fatalf("ParseCodexUsagePayload returned error: %v", err)
	}
	if parsed.ResetCreditsAvailableCount == nil || *parsed.ResetCreditsAvailableCount != 2 {
		t.Fatalf("reset credits count = %#v", parsed.ResetCreditsAvailableCount)
	}
	if len(parsed.ResetCredits) != 2 {
		t.Fatalf("reset credits = %#v, want 2", parsed.ResetCredits)
	}
	if !parsed.ResetCredits[0].ExpiresAt.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("first expiry = %s", parsed.ResetCredits[0].ExpiresAt)
	}
}

func TestParseResetCreditsPayloadUsesEndpointShapeAndInfersThirtyDayExpiry(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "available_count": 2,
	  "total_earned_count": 3,
	  "credits": [
	    {"id": "credit-1", "status": "available", "granted_at": "2026-06-01T00:00:00Z", "expires_at": "2026-07-01T00:00:00Z"},
	    {"id": "credit-2", "status": "available", "granted_at": "2026-06-05T00:00:00Z"}
	  ]
	}`)
	parsed, err := ParseResetCreditsPayload(raw, now)
	if err != nil {
		t.Fatalf("ParseResetCreditsPayload returned error: %v", err)
	}
	if parsed.ResetCreditsAvailableCount == nil || *parsed.ResetCreditsAvailableCount != 2 {
		t.Fatalf("available count = %#v", parsed.ResetCreditsAvailableCount)
	}
	if parsed.ResetCreditsTotalEarnedCount == nil || *parsed.ResetCreditsTotalEarnedCount != 3 {
		t.Fatalf("total earned count = %#v", parsed.ResetCreditsTotalEarnedCount)
	}
	if len(parsed.ResetCredits) != 2 {
		t.Fatalf("reset credits = %#v", parsed.ResetCredits)
	}
	if parsed.ResetCredits[0].ID != "credit-1" || parsed.ResetCredits[0].Status != "available" {
		t.Fatalf("first credit = %#v", parsed.ResetCredits[0])
	}
	if !parsed.ResetCredits[1].ExpiresAt.Equal(time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("inferred expiry = %s", parsed.ResetCredits[1].ExpiresAt)
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
	if parsed.LongWindow == nil || parsed.LongWindow.Exhausted {
		t.Fatalf("long window = %#v, want not exhausted when weekly usage remains", parsed.LongWindow)
	}
}

func TestParseCodexUsageCodeReviewWeeklyWindows(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "rate_limit": {
	    "primary_window": {"used_percent": 10, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
	    "secondary_window": {"used_percent": 30, "limit_window_seconds": 2592000, "reset_after_seconds": 172800}
	  },
	  "code_review_rate_limit": {
	    "primary_window": {"used_percent": 40, "limit_window_seconds": 18000, "reset_after_seconds": 7200},
	    "secondary_window": {"used_percent": 60, "limit_window_seconds": 604800, "reset_after_seconds": 86400}
	  }
	}`)
	parsed, err := ParseCodexUsagePayload(raw, now)
	if err != nil {
		t.Fatalf("ParseCodexUsagePayload returned error: %v", err)
	}
	if parsed.Family != AccountFamilyMonthly {
		t.Fatalf("Family = %q, want top-level monthly", parsed.Family)
	}
	if parsed.LongWindow == nil || parsed.LongWindow.Kind != WindowMonthly {
		t.Fatalf("top-level long window = %#v, want monthly", parsed.LongWindow)
	}
	if len(parsed.CodeReviewWindows) != 2 {
		t.Fatalf("CodeReviewWindows length = %d, want 2: %#v", len(parsed.CodeReviewWindows), parsed.CodeReviewWindows)
	}
	if parsed.CodeReviewWindows[0].Kind != WindowFiveHour || parsed.CodeReviewWindows[1].Kind != WindowWeekly {
		t.Fatalf("CodeReviewWindows = %#v, want five-hour and weekly", parsed.CodeReviewWindows)
	}
	if !parsed.CodeReviewWindows[1].ResetAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("code review weekly reset = %s, want %s", parsed.CodeReviewWindows[1].ResetAt, now.Add(24*time.Hour))
	}
}

func TestParseCodexUsageCodeReviewMonthlyCamelCaseWindows(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "rateLimit": {
	    "primaryWindow": {"usedPercent": 10, "limitWindowSeconds": 18000, "resetAfterSeconds": 3600},
	    "secondaryWindow": {"usedPercent": 20, "limitWindowSeconds": 604800, "resetAfterSeconds": 86400}
	  },
	  "codeReviewRateLimit": {
	    "primaryWindow": {"usedPercent": 45, "limitWindowSeconds": 18000, "resetAfterSeconds": 7200},
	    "secondaryWindow": {"usedPercent": 70, "limitWindowSeconds": 2592000, "resetAfterSeconds": 172800}
	  }
	}`)
	parsed, err := ParseCodexUsagePayload(raw, now)
	if err != nil {
		t.Fatalf("ParseCodexUsagePayload returned error: %v", err)
	}
	if parsed.Family != AccountFamilyWeekly {
		t.Fatalf("Family = %q, want top-level weekly", parsed.Family)
	}
	if len(parsed.CodeReviewWindows) != 2 {
		t.Fatalf("CodeReviewWindows length = %d, want 2: %#v", len(parsed.CodeReviewWindows), parsed.CodeReviewWindows)
	}
	if parsed.CodeReviewWindows[0].Kind != WindowFiveHour || parsed.CodeReviewWindows[1].Kind != WindowMonthly {
		t.Fatalf("CodeReviewWindows = %#v, want five-hour and monthly", parsed.CodeReviewWindows)
	}
	if !parsed.CodeReviewWindows[1].ResetAt.Equal(now.Add(48 * time.Hour)) {
		t.Fatalf("code review monthly reset = %s, want %s", parsed.CodeReviewWindows[1].ResetAt, now.Add(48*time.Hour))
	}
}

func TestParseCodexUsageAdditionalRateLimitWindows(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	raw := []byte(`{
	  "rate_limit": {
	    "primary_window": {"used_percent": 10, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
	    "secondary_window": {"used_percent": 20, "limit_window_seconds": 604800, "reset_after_seconds": 86400}
	  },
	  "additional_rate_limits": [
	    {
	      "rate_limit": {
	        "primary_window": {"used_percent": 33, "limit_window_seconds": 18000, "reset_after_seconds": 1800},
	        "secondary_window": {"used_percent": 66, "limit_window_seconds": 2592000, "reset_after_seconds": 259200}
	      }
	    }
	  ]
	}`)
	parsed, err := ParseCodexUsagePayload(raw, now)
	if err != nil {
		t.Fatalf("ParseCodexUsagePayload returned error: %v", err)
	}
	if parsed.Family != AccountFamilyWeekly {
		t.Fatalf("Family = %q, want top-level weekly", parsed.Family)
	}
	if len(parsed.AdditionalWindows) != 2 {
		t.Fatalf("AdditionalWindows length = %d, want 2: %#v", len(parsed.AdditionalWindows), parsed.AdditionalWindows)
	}
	if parsed.AdditionalWindows[0].Kind != WindowFiveHour || parsed.AdditionalWindows[1].Kind != WindowMonthly {
		t.Fatalf("AdditionalWindows = %#v, want five-hour and monthly", parsed.AdditionalWindows)
	}
	if !parsed.AdditionalWindows[1].ResetAt.Equal(now.Add(72 * time.Hour)) {
		t.Fatalf("additional monthly reset = %s, want %s", parsed.AdditionalWindows[1].ResetAt, now.Add(72*time.Hour))
	}
}
