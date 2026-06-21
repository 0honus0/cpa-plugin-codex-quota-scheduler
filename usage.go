package main

import (
	"encoding/json"
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
	ResetsAt        string               `json:"resets_at"`
	ResetsInSeconds *int64               `json:"resets_in_seconds"`
	Error           *quotaFailureDetails `json:"error"`
}

type quotaFailureDetails struct {
	Type            string `json:"type"`
	ResetsAt        string `json:"resets_at"`
	ResetsInSeconds *int64 `json:"resets_in_seconds"`
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
	if body.Type == usageLimitReachedReason {
		return true
	}
	return body.Error != nil && body.Error.Type == usageLimitReachedReason
}

func quotaFailureResetAt(body quotaFailureBody, now time.Time) (time.Time, bool) {
	if body.Error != nil {
		if resetAt, ok := parseResetAt(body.Error.ResetsAt); ok {
			return resetAt, true
		}
		if body.Error.ResetsInSeconds != nil {
			return now.Add(time.Duration(*body.Error.ResetsInSeconds) * time.Second), true
		}
	}
	if resetAt, ok := parseResetAt(body.ResetsAt); ok {
		return resetAt, true
	}
	if body.ResetsInSeconds != nil {
		return now.Add(time.Duration(*body.ResetsInSeconds) * time.Second), true
	}
	return time.Time{}, false
}

func parseResetAt(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	resetAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return resetAt, true
}
