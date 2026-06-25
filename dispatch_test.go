package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestLogSchedulerDecisionIncludesDetailedFallbackReason(t *testing.T) {
	now := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	decision := PickDecision{
		Handled:         true,
		DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst,
		Reason:          "fallback_fill_first",
		Ordered: []ScheduledAccount{
			{AuthID: "five-hour", QueueStatus: QueueStatusFiveHourExhausted, UnavailableReason: "five_hour_exhausted"},
			{AuthID: "weekly", QueueStatus: QueueStatusLongWindowExhausted, UnavailableReason: "weekly_exhausted"},
		},
	}

	logSchedulerDecision(store, pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-5-codex"}, decision, now)

	logs := store.Snapshot(now).Logs
	if len(logs) != 1 {
		t.Fatalf("logs len = %d, want 1", len(logs))
	}
	fields := logs[0].Fields
	if fields["reason"] != "fallback_fill_first" || fields["fallback"] != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("fields = %#v, want fallback reason and builtin", fields)
	}
	if fields["ordered_count"] != 2 {
		t.Fatalf("ordered_count = %#v, want 2; fields=%#v", fields["ordered_count"], fields)
	}
	if fields["unavailable_summary"] == "" {
		t.Fatalf("unavailable_summary empty; fields=%#v", fields)
	}
}

func TestLogSchedulerDecisionIncludesSelectedAccountContext(t *testing.T) {
	now := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	decision := PickDecision{
		AuthID:  "auth-1",
		Handled: true,
		Reason:  "selected",
		Ordered: []ScheduledAccount{
			{AuthID: "auth-1", QueueStatus: QueueStatusAvailable, Available: true},
		},
	}

	logSchedulerDecision(store, pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-5-codex"}, decision, now)

	fields := store.Snapshot(now).Logs[0].Fields
	if fields["auth_id"] != "auth-1" || fields["selected_queue_status"] != string(QueueStatusAvailable) || fields["ordered_count"] != 1 {
		t.Fatalf("fields = %#v, want selected account context", fields)
	}
}
