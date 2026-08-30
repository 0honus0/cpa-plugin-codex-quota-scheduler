package main

import (
	"encoding/json"
	"time"
)

const (
	fiveHourSeconds     = 18000
	weekSeconds         = 604800
	minMonthSeconds     = 2419200
	maxMonthSeconds     = 2678400
	resetCreditValidity = 30 * 24 * time.Hour
)

func ParseCodexUsagePayload(raw []byte, now time.Time) (ParsedQuota, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ParsedQuota{}, err
	}

	parsed := ParsedQuota{
		Family:   AccountFamilyUnknown,
		PlanType: getString(doc, "plan_type", "planType"),
	}
	if credits, ok := getMap(doc, "rate_limit_reset_credits", "rateLimitResetCredits"); ok {
		if count, ok := getInt(credits, "available_count", "availableCount"); ok {
			parsed.ResetCreditsAvailableCount = &count
		}
		parsed.ResetCredits = parseResetCredits(credits)
	}

	if codeReviewRateLimit, ok := getMap(doc, "code_review_rate_limit", "codeReviewRateLimit"); ok {
		parsed.CodeReviewWindows = append(parsed.CodeReviewWindows, parseRateLimitWindows(codeReviewRateLimit, now)...)
	}
	if additionalLimits, ok := getSlice(doc, "additional_rate_limits", "additionalRateLimits"); ok {
		for _, rawLimit := range additionalLimits {
			limit, ok := rawLimit.(map[string]any)
			if !ok {
				continue
			}
			rateLimit, ok := getMap(limit, "rate_limit", "rateLimit")
			if !ok {
				continue
			}
			parsed.AdditionalWindows = append(parsed.AdditionalWindows, parseRateLimitWindows(rateLimit, now)...)
		}
	}

	rateLimit, ok := getMap(doc, "rate_limit", "rateLimit")
	if !ok {
		return parsed, nil
	}
	for _, window := range parseRateLimitWindows(rateLimit, now) {
		window := window
		switch window.Kind {
		case WindowFiveHour:
			parsed.FiveHour = &window
		case WindowWeekly, WindowMonthly:
			parsed.LongWindow = &window
			if window.Kind == WindowWeekly {
				parsed.Family = AccountFamilyWeekly
			} else {
				parsed.Family = AccountFamilyMonthly
			}
		default:
			parsed.AdditionalWindows = append(parsed.AdditionalWindows, window)
		}
	}

	return parsed, nil
}

func ParseResetCreditsPayload(raw []byte, now time.Time) (ParsedQuota, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ParsedQuota{}, err
	}
	parsed := ParsedQuota{Family: AccountFamilyUnknown}
	if count, ok := getInt(doc, "available_count", "availableCount"); ok {
		parsed.ResetCreditsAvailableCount = &count
	}
	if count, ok := getInt(doc, "total_earned_count", "totalEarnedCount"); ok {
		parsed.ResetCreditsTotalEarnedCount = &count
	}
	parsed.ResetCredits = parseResetCredits(doc)
	return parsed, nil
}

func parseResetCredits(raw map[string]any) []ResetCredit {
	for _, key := range []string{"credits", "items", "list", "reset_credits", "resetCredits"} {
		value, ok := raw[key]
		if !ok {
			continue
		}
		items, ok := value.([]any)
		if !ok {
			continue
		}
		credits := make([]ResetCredit, 0, len(items))
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if credit, ok := parseResetCredit(m); ok {
				credits = append(credits, credit)
			}
		}
		return credits
	}
	if credit, ok := parseResetCredit(raw); ok {
		return []ResetCredit{credit}
	}
	return nil
}

func parseResetCredit(raw map[string]any) (ResetCredit, bool) {
	credit := ResetCredit{
		ID:     getString(raw, "id"),
		Status: getString(raw, "status"),
	}
	if grantedAt, ok := getTime(raw, "granted_at", "grantedAt", "created_at", "createdAt"); ok {
		credit.GrantedAt = grantedAt
	}
	if expiresAt, ok := getTime(raw, "expires_at", "expiresAt", "expire_at", "expireAt", "expiration", "expires"); ok {
		credit.ExpiresAt = expiresAt
	} else if !credit.GrantedAt.IsZero() {
		credit.ExpiresAt = credit.GrantedAt.Add(resetCreditValidity)
	}
	if usedAt, ok := getTime(raw, "used_at", "usedAt"); ok {
		credit.UsedAt = usedAt
	}
	return credit, !credit.ExpiresAt.IsZero() || credit.ID != "" || credit.Status != ""
}

func parseRateLimitWindows(rateLimit map[string]any, now time.Time) []QuotaWindow {
	allowed, allowedOK := getBool(rateLimit, "allowed")
	limitReached, _ := getBool(rateLimit, "limit_reached", "limitReached")
	exhaustedByEnvelope := (allowedOK && !allowed) || limitReached

	var windows []QuotaWindow
	if primary, ok := getMap(rateLimit, "primary_window", "primaryWindow"); ok {
		windows = append(windows, parseQuotaWindow(primary, now, WindowFiveHour, exhaustedByEnvelope))
	}
	if secondary, ok := getMap(rateLimit, "secondary_window", "secondaryWindow"); ok {
		windows = append(windows, parseQuotaWindow(secondary, now, WindowWeekly, exhaustedByEnvelope))
	}
	for i := range windows {
		classifyWindow(&windows[i], i)
	}
	return windows
}

func parseQuotaWindow(raw map[string]any, now time.Time, fallbackKind WindowKind, exhaustedByEnvelope bool) QuotaWindow {
	window := QuotaWindow{Kind: fallbackKind}
	hasWindowExhaustionSignal := false
	if usedPercent, ok := getFloat64(raw, "used_percent", "usedPercent"); ok {
		hasWindowExhaustionSignal = true
		window.UsedPercent = &usedPercent
		if usedPercent >= 100 {
			window.Exhausted = true
		}
	}
	if allowed, ok := getBool(raw, "allowed"); ok {
		hasWindowExhaustionSignal = true
		if !allowed {
			window.Exhausted = true
		}
	}
	if limitReached, ok := getBool(raw, "limit_reached", "limitReached"); ok {
		hasWindowExhaustionSignal = true
		if limitReached {
			window.Exhausted = true
		}
	}
	if !hasWindowExhaustionSignal && exhaustedByEnvelope {
		window.Exhausted = true
	}
	if seconds, ok := getInt64(raw, "limit_window_seconds", "limitWindowSeconds"); ok {
		window.LimitWindowSeconds = &seconds
	}
	if resetAt, ok := getResetAt(raw, now); ok {
		window.ResetAt = resetAt
	}
	return window
}

func classifyWindow(window *QuotaWindow, order int) {
	if window.LimitWindowSeconds == nil {
		if order == 0 {
			window.Kind = WindowFiveHour
		} else if order == 1 {
			window.Kind = WindowWeekly
		}
		return
	}
	switch seconds := *window.LimitWindowSeconds; {
	case seconds == fiveHourSeconds:
		window.Kind = WindowFiveHour
	case seconds == weekSeconds:
		window.Kind = WindowWeekly
	case seconds >= minMonthSeconds && seconds <= maxMonthSeconds:
		window.Kind = WindowMonthly
	}
}

func getResetAt(m map[string]any, now time.Time) (time.Time, bool) {
	if raw, ok := getAny(m, "reset_at", "resetAt"); ok {
		switch v := raw.(type) {
		case string:
			if parsed, err := time.Parse(time.RFC3339, v); err == nil {
				return parsed, true
			}
		}
	}
	if seconds, ok := getFloat64(m, "reset_after_seconds", "resetAfterSeconds"); ok {
		return now.Add(time.Duration(seconds * float64(time.Second))), true
	}
	return time.Time{}, false
}

func getTime(m map[string]any, keys ...string) (time.Time, bool) {
	raw, ok := getAny(m, keys...)
	if !ok {
		return time.Time{}, false
	}
	switch v := raw.(type) {
	case string:
		if parsed, err := time.Parse(time.RFC3339, v); err == nil {
			return parsed, true
		}
	case float64:
		return time.Unix(int64(v), 0).UTC(), true
	case int64:
		return time.Unix(v, 0).UTC(), true
	case int:
		return time.Unix(int64(v), 0).UTC(), true
	}
	return time.Time{}, false
}

func getMap(m map[string]any, keys ...string) (map[string]any, bool) {
	raw, ok := getAny(m, keys...)
	if !ok {
		return nil, false
	}
	child, ok := raw.(map[string]any)
	return child, ok
}

func getSlice(m map[string]any, keys ...string) ([]any, bool) {
	raw, ok := getAny(m, keys...)
	if !ok {
		return nil, false
	}
	value, ok := raw.([]any)
	return value, ok
}

func getString(m map[string]any, keys ...string) string {
	raw, ok := getAny(m, keys...)
	if !ok {
		return ""
	}
	value, _ := raw.(string)
	return value
}

func getBool(m map[string]any, keys ...string) (bool, bool) {
	raw, ok := getAny(m, keys...)
	if !ok {
		return false, false
	}
	value, ok := raw.(bool)
	return value, ok
}

func getInt(m map[string]any, keys ...string) (int, bool) {
	raw, ok := getAny(m, keys...)
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}

func getInt64(m map[string]any, keys ...string) (int64, bool) {
	raw, ok := getAny(m, keys...)
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}

func getFloat64(m map[string]any, keys ...string) (float64, bool) {
	raw, ok := getAny(m, keys...)
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

func getAny(m map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			return value, true
		}
	}
	return nil, false
}
