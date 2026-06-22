package main

import (
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	PluginID = "codex-quota-scheduler"

	MonthlyModePriority    MonthlyMode = "priority"
	MonthlyModeExpiryOrder MonthlyMode = "expiry_order"

	FallbackFillFirst FallbackMode = "fill-first"
)

type MonthlyMode string

type FallbackMode string

type Config struct {
	HandleEnabled         bool
	QuotaRefreshInterval  time.Duration
	StaleAfter            time.Duration
	MonthlyMode           MonthlyMode
	Fallback              FallbackMode
	EnableUsageFeedback   bool
	MaxRefreshConcurrency int
	QuotaEndpoint         string
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	Scheduler     bool `json:"scheduler"`
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

type rawConfig struct {
	HandleEnabled         *bool  `yaml:"handle_enabled"`
	QuotaRefreshInterval  string `yaml:"quota_refresh_interval"`
	StaleAfter            string `yaml:"stale_after"`
	MonthlyMode           string `yaml:"monthly_mode"`
	Fallback              string `yaml:"fallback"`
	EnableUsageFeedback   *bool  `yaml:"enable_usage_feedback"`
	MaxRefreshConcurrency *int   `yaml:"max_refresh_concurrency"`
	QuotaEndpoint         string `yaml:"quota_endpoint"`
}

func DefaultConfig() Config {
	return Config{
		HandleEnabled:         true,
		QuotaRefreshInterval:  time.Minute,
		StaleAfter:            10 * time.Minute,
		MonthlyMode:           MonthlyModeExpiryOrder,
		Fallback:              FallbackFillFirst,
		EnableUsageFeedback:   true,
		MaxRefreshConcurrency: 4,
		QuotaEndpoint:         "https://chatgpt.com/backend-api/wham/usage",
	}
}

func DecodeConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(raw) == 0 {
		return cfg, nil
	}

	var decoded rawConfig
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		return Config{}, err
	}
	if decoded.HandleEnabled != nil {
		cfg.HandleEnabled = *decoded.HandleEnabled
	}
	if decoded.QuotaRefreshInterval != "" {
		d, err := time.ParseDuration(decoded.QuotaRefreshInterval)
		if err != nil {
			return Config{}, fmt.Errorf("quota_refresh_interval: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("quota_refresh_interval must be positive")
		}
		cfg.QuotaRefreshInterval = d
	}
	if decoded.StaleAfter != "" {
		d, err := time.ParseDuration(decoded.StaleAfter)
		if err != nil {
			return Config{}, fmt.Errorf("stale_after: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("stale_after must be positive")
		}
		cfg.StaleAfter = d
	}
	if decoded.MonthlyMode != "" {
		cfg.MonthlyMode = MonthlyMode(decoded.MonthlyMode)
	}
	if cfg.MonthlyMode != MonthlyModePriority && cfg.MonthlyMode != MonthlyModeExpiryOrder {
		return Config{}, fmt.Errorf("monthly_mode must be %q or %q", MonthlyModePriority, MonthlyModeExpiryOrder)
	}
	if decoded.Fallback != "" {
		cfg.Fallback = FallbackMode(decoded.Fallback)
	}
	if cfg.Fallback != "" && cfg.Fallback != FallbackFillFirst {
		return Config{}, fmt.Errorf("fallback must be empty or %q", FallbackFillFirst)
	}
	if decoded.EnableUsageFeedback != nil {
		cfg.EnableUsageFeedback = *decoded.EnableUsageFeedback
	}
	if decoded.MaxRefreshConcurrency != nil {
		if *decoded.MaxRefreshConcurrency <= 0 {
			return Config{}, fmt.Errorf("max_refresh_concurrency must be positive")
		}
		cfg.MaxRefreshConcurrency = *decoded.MaxRefreshConcurrency
	}
	if decoded.QuotaEndpoint != "" {
		cfg.QuotaEndpoint = decoded.QuotaEndpoint
	}
	return cfg, nil
}

func PluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             PluginID,
			Version:          "0.1.0",
			Author:           "Jeffery",
			GitHubRepository: "https://github.com/jeffery/codex-quota-scheduler",
			Logo:             "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/main/docs/logo.png",
		},
		Capabilities: registrationCapabilities{
			Scheduler:     true,
			UsagePlugin:   true,
			ManagementAPI: true,
		},
	}
}
