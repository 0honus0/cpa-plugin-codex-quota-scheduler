package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadPluginDiskStateResetsLegacyNonChatGPTQuotaEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := []byte(`{
  "config": {
    "HandleEnabled": true,
    "QuotaRefreshInterval": 1800000000000,
    "StaleAfter": 18000000000000,
    "MonthlyMode": "expiry_order",
    "Fallback": "fill-first",
    "EnableUsageFeedback": true,
    "MaxRefreshConcurrency": 1,
    "QuotaEndpoint": "https://example.test/usage",
    "CircuitFailureThreshold": 3,
    "CircuitOpenDuration": 600000000000,
    "CircuitHalfOpenSuccessThreshold": 1,
    "MaxLogEntries": 2000,
    "LogRetention": 86400000000000
  }
}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	state, err := LoadPluginDiskState(path)
	if err != nil {
		t.Fatalf("LoadPluginDiskState returned error: %v", err)
	}
	if state.Config.QuotaEndpoint != chatGPTQuotaEndpoint {
		t.Fatalf("QuotaEndpoint = %q, want %q", state.Config.QuotaEndpoint, chatGPTQuotaEndpoint)
	}
	if state.Config.QuotaRefreshInterval != 30*time.Minute {
		t.Fatalf("QuotaRefreshInterval = %s, want 30m", state.Config.QuotaRefreshInterval)
	}
}
