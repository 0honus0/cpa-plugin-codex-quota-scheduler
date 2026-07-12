package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// inv:INV-43,INV-44
func TestSchedulerPickABIPathSnapshotOnly(t *testing.T) {
	now := time.Now()
	s := &SchedulerSnapshot{HandleEnabled: true, Accounts: []AccountView{{ID: "a", Instance: 1, Cache: CacheFresh}}, ActiveHighestTier: map[string]struct{}{"a": {}}}
	PublishSchedulerSnapshot(s)
	d := schedulerPickPublished(pluginapi.SchedulerPickRequest{Provider: "codex", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}}}, now)
	if d.AuthID != "a" {
		t.Fatalf("selected %q", d.AuthID)
	}
}
