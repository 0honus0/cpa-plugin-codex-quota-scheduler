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

func TestDetectQuotaFailureNumericErrorResetAt(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":1782043200}}`,
		},
	}

	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(time.Unix(1782043200, 0).UTC()) {
		t.Fatalf("ResetAt = %s, want %s", event.ResetAt, time.Unix(1782043200, 0).UTC())
	}
}

func TestDetectQuotaFailureNumericTopLevelResetAt(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached"},"resets_at":1782043200}`,
		},
	}

	event, ok := DetectQuotaFailure(record, now)
	if !ok {
		t.Fatalf("DetectQuotaFailure did not detect quota failure")
	}
	if !event.ResetAt.Equal(time.Unix(1782043200, 0).UTC()) {
		t.Fatalf("ResetAt = %s, want %s", event.ResetAt, time.Unix(1782043200, 0).UTC())
	}
}

func TestDetectQuotaFailureInvalidResetAtUsesResetsInSeconds(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":"not-a-time","resets_in_seconds":120}}`,
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

func TestDetectQuotaFailureInvalidResetFieldsFallsBack(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_at":"not-a-time","resets_in_seconds":"later"}}`,
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

func TestDetectQuotaFailureIgnoresMalformedBody(t *testing.T) {
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `not-json`,
		},
	}
	if event, ok := DetectQuotaFailure(record, now); ok {
		t.Fatalf("unexpected event = %#v", event)
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

func TestUsageHandleOpensCircuitAfterThresholdByAuthIndex(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CircuitFailureThreshold = 2
	cfg.CircuitOpenDuration = 10 * time.Minute
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
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	HandleUsageFeedback(store, record, now)
	snapshot := store.Snapshot(now)
	if snapshot.Accounts[0].TemporaryExhausted || snapshot.Accounts[0].Circuit.State != CircuitStateClosed || snapshot.Accounts[0].Circuit.FailureCount != 1 {
		t.Fatalf("account after first failure = %#v", snapshot.Accounts[0])
	}
	status, available, reason, sortTime := accountQueueState(snapshot.Accounts[0], now)
	if status != QueueStatusUnavailable || available || reason != "quota_probe_wait" || !sortTime.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("queue after first failure = status=%s available=%t reason=%q sort=%s account=%#v", status, available, reason, sortTime, snapshot.Accounts[0])
	}

	HandleUsageFeedback(store, record, now.Add(time.Second))

	snapshot = store.Snapshot(now.Add(time.Second))
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("accounts = %#v", snapshot.Accounts)
	}
	account := snapshot.Accounts[0]
	if account.TemporaryExhausted {
		t.Fatalf("temporary exhausted should no longer be set by usage feedback: %#v", account)
	}
	if account.Circuit.State != CircuitStateOpen || account.Circuit.FailureCount != 2 || !account.Circuit.NextProbeAt.Equal(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("account = %#v", account)
	}
}

func TestUsageCircuitHalfOpenSuccessClosesCircuit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CircuitFailureThreshold = 1
	cfg.CircuitOpenDuration = 5 * time.Minute
	cfg.CircuitHalfOpenSuccessThreshold = 2
	store := NewPluginState(cfg)
	account := weeklyAccount("auth-1", 5, time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC), false)
	store.UpsertQuota(account)

	fail := pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-1",
		Failed:   true,
		Failure: pluginapi.UsageFailure{
			StatusCode: 429,
			Body:       `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`,
		},
	}
	now := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	HandleUsageFeedback(store, fail, now)

	snapshot := store.Snapshot(now.Add(6 * time.Minute))
	if snapshot.Accounts[0].Circuit.EffectiveState != CircuitStateHalfOpen {
		t.Fatalf("circuit after probe time = %#v", snapshot.Accounts[0].Circuit)
	}

	success := pluginapi.UsageRecord{Provider: "codex", AuthID: "auth-1", Failed: false}
	HandleUsageFeedback(store, success, now.Add(6*time.Minute))
	snapshot = store.Snapshot(now.Add(6 * time.Minute))
	if snapshot.Accounts[0].Circuit.EffectiveState != CircuitStateHalfOpen || snapshot.Accounts[0].Circuit.SuccessCount != 1 {
		t.Fatalf("circuit after first success = %#v", snapshot.Accounts[0].Circuit)
	}

	HandleUsageFeedback(store, success, now.Add(6*time.Minute+time.Second))
	snapshot = store.Snapshot(now.Add(6*time.Minute + time.Second))
	if snapshot.Accounts[0].Circuit.EffectiveState != CircuitStateClosed || snapshot.Accounts[0].Circuit.FailureCount != 0 || snapshot.Accounts[0].Circuit.SuccessCount != 0 {
		t.Fatalf("circuit after second success = %#v", snapshot.Accounts[0].Circuit)
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
