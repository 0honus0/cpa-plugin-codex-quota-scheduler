package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	usageLimitReachedReason = "usage_limit_reached"
	usageLimitNoResetReason = "usage_limit_reached_no_reset"
)

type QuotaFailureEvent struct {
	AuthID    string
	AuthIndex string
	ResetAt   time.Time
	Reason    string
}

type quotaFailureBody struct {
	Type            string               `json:"type"`
	ResetsAt        json.RawMessage      `json:"resets_at"`
	ResetsInSeconds json.RawMessage      `json:"resets_in_seconds"`
	Error           *quotaFailureDetails `json:"error"`
}

type quotaFailureDetails struct {
	Type            string          `json:"type"`
	ResetsAt        json.RawMessage `json:"resets_at"`
	ResetsInSeconds json.RawMessage `json:"resets_in_seconds"`
}

func DetectQuotaFailure(record pluginapi.UsageRecord, now time.Time) (QuotaFailureEvent, bool) {
	if record.Provider != "codex" || !record.Failed || record.Failure.StatusCode != 429 {
		return QuotaFailureEvent{}, false
	}

	var body quotaFailureBody
	if err := json.Unmarshal([]byte(record.Failure.Body), &body); err != nil {
		return QuotaFailureEvent{}, false
	}
	if !isUsageLimitReached(body) {
		return QuotaFailureEvent{}, false
	}

	event := QuotaFailureEvent{
		AuthID:    record.AuthID,
		AuthIndex: record.AuthIndex,
		Reason:    usageLimitReachedReason,
	}
	if resetAt, ok := quotaFailureResetAt(body, now); ok {
		event.ResetAt = resetAt
		return event, true
	}

	event.ResetAt = now.Add(2 * time.Minute)
	event.Reason = usageLimitNoResetReason
	return event, true
}

func HandleUsageFeedback(state *PluginState, record pluginapi.UsageRecord, now time.Time) {
	if state == nil || !state.Config().EnableUsageFeedback {
		return
	}
	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		return
	}
	if event.AuthID != "" {
		state.MarkAccountTemporaryExhausted(event.AuthID, event.ResetAt, event.Reason)
	}
	if event.AuthIndex != "" {
		state.MarkAccountTemporaryExhaustedByAuthIndex(event.AuthIndex, event.ResetAt, event.Reason)
	}
}

func isUsageLimitReached(body quotaFailureBody) bool {
	if equalUsageFailureType(body.Type, usageLimitReachedReason) {
		return true
	}
	return body.Error != nil && equalUsageFailureType(body.Error.Type, usageLimitReachedReason)
}

func quotaFailureResetAt(body quotaFailureBody, now time.Time) (time.Time, bool) {
	if body.Error != nil {
		if resetAt, ok := parseResetAt(body.Error.ResetsAt); ok {
			return resetAt, true
		}
		if resetAfter, ok := parseResetSeconds(body.Error.ResetsInSeconds); ok {
			return now.Add(resetAfter), true
		}
	}
	if resetAt, ok := parseResetAt(body.ResetsAt); ok {
		return resetAt, true
	}
	if resetAfter, ok := parseResetSeconds(body.ResetsInSeconds); ok {
		return now.Add(resetAfter), true
	}
	return time.Time{}, false
}

func equalUsageFailureType(got, want string) bool {
	return strings.EqualFold(strings.TrimSpace(got), want)
}

func parseResetAt(raw json.RawMessage) (time.Time, bool) {
	value, ok := parseJSONScalar(raw)
	if !ok {
		return time.Time{}, false
	}
	if resetAt, err := time.Parse(time.RFC3339, value); err == nil {
		return resetAt, true
	}
	unixSeconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(unixSeconds, 0).UTC(), true
}

func parseResetSeconds(raw json.RawMessage) (time.Duration, bool) {
	value, ok := parseJSONScalar(raw)
	if !ok {
		return 0, false
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func parseJSONScalar(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		text = strings.TrimSpace(text)
		return text, text != ""
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err == nil {
		return number.String(), true
	}
	return "", false
}
