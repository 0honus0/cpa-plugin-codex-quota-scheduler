package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDetectQuotaFailureUsageLimitReachedWithResetAt(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","message":"limit","resets_at":"2026-06-21T12:00:00Z"}}`,
		},
	}
	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if event.AuthID != "auth-1" || !event.ResetAt.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("event = %#v", event)
	}
}

func TestDetectQuotaFailureResetsInSeconds(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`,
		},
	}
	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(now.Add(120 * time.Second)) {
		t.Fatalf("ResetAt = %s, want %s", event.ResetAt, now.Add(120*time.Second))
	}
}

func TestDetectQuotaFailureIgnoresGenericRateLimit(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"rate_limit_error"}}`,
		},
	}
	if event, ok := DetectQuotaFailure(record, now); ok {
		t.Fatalf("unexpected event = %#v", event)
	}
}

func TestDetectQuotaFailureTopLevelResetFields(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider:  "codex",
		AuthID:    "auth-1",
		AuthIndex: "idx-1",
		Failed:    true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached"},"resets_at":"2026-06-21T12:00:00Z"}`,
		},
	}

	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if event.AuthIndex != "idx-1" || !event.ResetAt.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("event = %#v", event)
	}
}

func TestDetectQuotaFailureUsesFallbackResetWhenMissing(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached"}}`,
		},
	}

	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(now.Add(2*time.Minute)) || event.Reason != "usage_limit_reached_no_reset" {
		t.Fatalf("event = %#v", event)
	}
}

func TestDetectQuotaFailureIgnoresNonCodexProvider(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "openai",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`,
		},
	}
	if event, ok := DetectQuotaFailure(record, now); ok {
		t.Fatalf("unexpected event = %#v", event)
	}
}

func TestDetectQuotaFailureIgnoresSuccessfulRecord(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   false,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`,
		},
	}
	if event, ok := DetectQuotaFailure(record, now); ok {
		t.Fatalf("unexpected event = %#v", event)
	}
}

func TestUsageHandleMarksTemporaryExhaustedByAuthIndex(t *testing.T) {
	cfg := DefaultConfig()
	store := NewPluginState(cfg)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})

	record := pluginapi.UsageRecord{
		Provider:  "codex",
		AuthIndex: "idx-1",
		Failed:    true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":"2026-06-21T12:00:00Z"}}`,
		},
	}
	HandleUsageFeedback(store, record, time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC))

	snapshot := store.Snapshot(time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC))
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %#v", snapshot.Accounts)
	}
	account := snapshot.Accounts[0]
	if !account.TemporaryExhausted || !account.TemporaryResetAt.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("account = %#v", account)
	}
}

func TestUsageHandleDisabledDoesNotMutateState(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnableUsageFeedback = false
	store := NewPluginState(cfg)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex"})

	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":"2026-06-21T12:00:00Z"}}`,
		},
	}
	HandleUsageFeedback(store, record, time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC))

	snapshot := store.Snapshot(time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC))
	if snapshot.Accounts[0].TemporaryExhausted {
		t.Fatalf("account = %#v", snapshot.Accounts[0])
	}
}
