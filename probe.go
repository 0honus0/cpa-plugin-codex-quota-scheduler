package main

import (
	"encoding/json"
	"time"
)

const (
	resetProbeAfterResetDelay = 10 * time.Minute
	resetProbeCloseThreshold  = 3 * time.Minute
	codexResetProbeEndpoint   = "https://chatgpt.com/backend-api/codex/responses/compact"
	codexResetProbeModel      = "gpt-5.4-mini"
	resetProbePayload         = `{"model":"gpt-5.4-mini","instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}]}`
)

func probeWindowDuration(window QuotaWindow) (time.Duration, bool) {
	if window.LimitWindowSeconds != nil && *window.LimitWindowSeconds > 0 {
		return time.Duration(*window.LimitWindowSeconds) * time.Second, true
	}
	switch window.Kind {
	case WindowFiveHour:
		return 5 * time.Hour, true
	case WindowWeekly:
		return 7 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

func scheduleResetProbesFromPrevious(previous AccountState, current map[WindowKind]ResetProbeState, now time.Time) map[WindowKind]ResetProbeState {
	next := cloneResetProbes(current)
	for _, window := range resetProbeCandidateWindows(previous.Quota) {
		duration, ok := probeWindowDuration(window)
		if !ok || duration <= 0 || window.ResetAt.IsZero() || window.ResetAt.After(now) {
			continue
		}
		if existing, ok := next[window.Kind]; ok && existing.ResetAt.Equal(window.ResetAt) {
			continue
		}
		if next == nil {
			next = make(map[WindowKind]ResetProbeState)
		}
		next[window.Kind] = ResetProbeState{
			WindowKind:    window.Kind,
			WindowSeconds: int64(duration / time.Second),
			ResetAt:       window.ResetAt,
			NextCheckAt:   window.ResetAt.Add(resetProbeAfterResetDelay),
			Status:        ResetProbeStatusPending,
		}
	}
	return next
}

func resetProbeCandidateWindows(quota ParsedQuota) []QuotaWindow {
	windows := make([]QuotaWindow, 0, 2)
	if quota.FiveHour != nil {
		windows = append(windows, *quota.FiveHour)
	}
	if quota.LongWindow != nil {
		windows = append(windows, *quota.LongWindow)
	}
	return windows
}

func resetProbeDue(probe ResetProbeState, now time.Time) bool {
	return probe.Status == ResetProbeStatusPending && !probe.NextCheckAt.IsZero() && !probe.NextCheckAt.After(now)
}

func resetProbeDueAny(probes map[WindowKind]ResetProbeState, now time.Time) bool {
	for _, probe := range probes {
		if resetProbeDue(probe, now) {
			return true
		}
	}
	return false
}

func looksLikeLazyReset(now time.Time, window QuotaWindow, windowSeconds int64) bool {
	if windowSeconds <= 0 || window.ResetAt.IsZero() {
		return false
	}
	duration := time.Duration(windowSeconds) * time.Second
	return absDuration(window.ResetAt.Sub(now.Add(duration))) <= resetProbeCloseThreshold
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func resetProbeUsageEvidence(body []byte) bool {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return false
	}
	for _, path := range [][]string{
		{"usage", "total_tokens"},
		{"usage", "prompt_tokens"},
		{"usage", "input_tokens"},
		{"usage", "completion_tokens"},
		{"usage", "output_tokens"},
		{"response", "usage", "total_tokens"},
		{"response", "usage", "prompt_tokens"},
		{"response", "usage", "input_tokens"},
		{"response", "usage", "completion_tokens"},
		{"response", "usage", "output_tokens"},
	} {
		if jsonNumberPathPositive(doc, path...) {
			return true
		}
	}
	return false
}

func jsonNumberPathPositive(root any, path ...string) bool {
	current := root
	for _, key := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = obj[key]
		if !ok {
			return false
		}
	}
	value, ok := current.(float64)
	return ok && value > 0
}

func resetProbePayloadBytes() []byte {
	return []byte(resetProbePayload)
}
