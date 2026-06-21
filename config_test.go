package main

import (
	"testing"
	"time"
)

func TestDecodeConfigDefaults(t *testing.T) {
	cfg, err := DecodeConfig(nil)
	if err != nil {
		t.Fatalf("DecodeConfig returned error: %v", err)
	}
	if !cfg.HandleEnabled {
		t.Fatalf("HandleEnabled = false, want true")
	}
	if cfg.MonthlyMode != MonthlyModeExpiryOrder {
		t.Fatalf("MonthlyMode = %q, want %q", cfg.MonthlyMode, MonthlyModeExpiryOrder)
	}
	if cfg.Fallback != FallbackFillFirst {
		t.Fatalf("Fallback = %q, want %q", cfg.Fallback, FallbackFillFirst)
	}
	if cfg.QuotaRefreshInterval != time.Minute {
		t.Fatalf("QuotaRefreshInterval = %s, want 1m", cfg.QuotaRefreshInterval)
	}
	if cfg.StaleAfter != 10*time.Minute {
		t.Fatalf("StaleAfter = %s, want 10m", cfg.StaleAfter)
	}
}

func TestDecodeConfigOverrides(t *testing.T) {
	raw := []byte(`
handle_enabled: false
quota_refresh_interval: 30s
stale_after: 2m
monthly_mode: priority
fallback: fill-first
enable_usage_feedback: false
annotation_state_path: C:\state\annotations.json
max_refresh_concurrency: 8
quota_endpoint: https://example.test/usage
`)
	cfg, err := DecodeConfig(raw)
	if err != nil {
		t.Fatalf("DecodeConfig returned error: %v", err)
	}
	if cfg.HandleEnabled {
		t.Fatalf("HandleEnabled = true, want false")
	}
	if cfg.MonthlyMode != MonthlyModePriority {
		t.Fatalf("MonthlyMode = %q, want %q", cfg.MonthlyMode, MonthlyModePriority)
	}
	if cfg.EnableUsageFeedback {
		t.Fatalf("EnableUsageFeedback = true, want false")
	}
	if cfg.MaxRefreshConcurrency != 8 {
		t.Fatalf("MaxRefreshConcurrency = %d, want 8", cfg.MaxRefreshConcurrency)
	}
}

func TestDecodeConfigRejectsInvalidMonthlyMode(t *testing.T) {
	_, err := DecodeConfig([]byte("monthly_mode: unsupported\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted invalid monthly mode")
	}
}

func TestPluginRegistrationDeclaresCapabilitiesAndFields(t *testing.T) {
	reg := PluginRegistration()
	if reg.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", reg.SchemaVersion)
	}
	if !reg.Capabilities.Scheduler || !reg.Capabilities.UsagePlugin || !reg.Capabilities.ManagementAPI {
		t.Fatalf("capabilities = %#v, want scheduler, usage_plugin, management_api", reg.Capabilities)
	}
	names := map[string]bool{}
	for _, field := range reg.Metadata.ConfigFields {
		names[field.Name] = true
	}
	for _, name := range []string{"handle_enabled", "monthly_mode", "quota_refresh_interval", "stale_after", "enable_usage_feedback"} {
		if !names[name] {
			t.Fatalf("ConfigFields missing %s", name)
		}
	}
}
