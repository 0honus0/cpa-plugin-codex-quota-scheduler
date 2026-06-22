package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
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
	if cfg.QuotaRefreshInterval != 30*time.Second {
		t.Fatalf("QuotaRefreshInterval = %s, want 30s", cfg.QuotaRefreshInterval)
	}
	if cfg.StaleAfter != 2*time.Minute {
		t.Fatalf("StaleAfter = %s, want 2m", cfg.StaleAfter)
	}
	if cfg.Fallback != FallbackFillFirst {
		t.Fatalf("Fallback = %q, want %q", cfg.Fallback, FallbackFillFirst)
	}
	if cfg.EnableUsageFeedback {
		t.Fatalf("EnableUsageFeedback = true, want false")
	}
	if cfg.MaxRefreshConcurrency != 8 {
		t.Fatalf("MaxRefreshConcurrency = %d, want 8", cfg.MaxRefreshConcurrency)
	}
	if cfg.QuotaEndpoint != "https://example.test/usage" {
		t.Fatalf("QuotaEndpoint = %q, want %q", cfg.QuotaEndpoint, "https://example.test/usage")
	}
}

func TestDecodeConfigRejectsInvalidMonthlyMode(t *testing.T) {
	_, err := DecodeConfig([]byte("monthly_mode: unsupported\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted invalid monthly mode")
	}
}

func TestDecodeConfigRejectsNonPositiveQuotaRefreshInterval(t *testing.T) {
	_, err := DecodeConfig([]byte("quota_refresh_interval: 0s\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted non-positive quota refresh interval")
	}
}

func TestDecodeConfigRejectsNonPositiveStaleAfter(t *testing.T) {
	_, err := DecodeConfig([]byte("stale_after: -1s\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted non-positive stale after")
	}
}

func TestDecodeConfigRejectsNonPositiveMaxRefreshConcurrency(t *testing.T) {
	_, err := DecodeConfig([]byte("max_refresh_concurrency: 0\n"))
	if err == nil {
		t.Fatalf("DecodeConfig accepted non-positive max refresh concurrency")
	}
}

func TestPluginRegistrationDeclaresCapabilitiesAndFields(t *testing.T) {
	reg := PluginRegistration()
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Name == "" || reg.Metadata.Version == "" || reg.Metadata.Author == "" || reg.Metadata.GitHubRepository == "" {
		t.Fatalf("metadata missing required CPA fields: %#v", reg.Metadata)
	}
	if len(reg.Metadata.ConfigFields) != 0 {
		t.Fatalf("ConfigFields len = %d, want 0; fields=%#v", len(reg.Metadata.ConfigFields), reg.Metadata.ConfigFields)
	}
	if !reg.Capabilities.Scheduler || !reg.Capabilities.UsagePlugin || !reg.Capabilities.ManagementAPI {
		t.Fatalf("capabilities = %#v, want scheduler, usage_plugin, management_api", reg.Capabilities)
	}
}
