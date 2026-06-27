package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	PluginID = "codex-quota-scheduler"

	chatGPTQuotaEndpoint = "https://chatgpt.com/backend-api/wham/usage"

	MonthlyModePriority    MonthlyMode = "priority"
	MonthlyModeExpiryOrder MonthlyMode = "expiry_order"

	FallbackFillFirst FallbackMode = "fill-first"
)

var pluginVersion = "0.1.1"

type MonthlyMode string

type FallbackMode string

type Config struct {
	HandleEnabled                   bool
	QuotaRefreshInterval            time.Duration
	StaleAfter                      time.Duration
	MonthlyMode                     MonthlyMode
	Fallback                        FallbackMode
	EnableUsageFeedback             bool
	MaxRefreshConcurrency           int
	QuotaEndpoint                   string
	CircuitFailureThreshold         int
	CircuitOpenDuration             time.Duration
	CircuitHalfOpenSuccessThreshold int
	MaxLogEntries                   int
	LogRetention                    time.Duration
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
	HandleEnabled                   *bool  `yaml:"handle_enabled"`
	QuotaRefreshInterval            string `yaml:"quota_refresh_interval"`
	StaleAfter                      string `yaml:"stale_after"`
	MonthlyMode                     string `yaml:"monthly_mode"`
	Fallback                        string `yaml:"fallback"`
	EnableUsageFeedback             *bool  `yaml:"enable_usage_feedback"`
	MaxRefreshConcurrency           *int   `yaml:"max_refresh_concurrency"`
	QuotaEndpoint                   string `yaml:"quota_endpoint"`
	CircuitFailureThreshold         *int   `yaml:"circuit_failure_threshold"`
	CircuitOpenDuration             string `yaml:"circuit_open_duration"`
	CircuitHalfOpenSuccessThreshold *int   `yaml:"circuit_half_open_success_threshold"`
	MaxLogEntries                   *int   `yaml:"max_log_entries"`
	LogRetention                    string `yaml:"log_retention"`
}

func DefaultConfig() Config {
	return Config{
		HandleEnabled:                   true,
		QuotaRefreshInterval:            30 * time.Minute,
		StaleAfter:                      5 * time.Hour,
		MonthlyMode:                     MonthlyModeExpiryOrder,
		Fallback:                        FallbackFillFirst,
		EnableUsageFeedback:             true,
		MaxRefreshConcurrency:           1,
		QuotaEndpoint:                   chatGPTQuotaEndpoint,
		CircuitFailureThreshold:         3,
		CircuitOpenDuration:             10 * time.Minute,
		CircuitHalfOpenSuccessThreshold: 1,
		MaxLogEntries:                   2000,
		LogRetention:                    24 * time.Hour,
	}
}

func NormalizeConfig(cfg Config) Config {
	defaults := DefaultConfig()
	if cfg.QuotaRefreshInterval <= 0 {
		cfg.QuotaRefreshInterval = defaults.QuotaRefreshInterval
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = defaults.StaleAfter
	}
	if cfg.MonthlyMode == "" {
		cfg.MonthlyMode = defaults.MonthlyMode
	}
	if cfg.Fallback == "" {
		cfg.Fallback = defaults.Fallback
	}
	if cfg.MaxRefreshConcurrency <= 0 {
		cfg.MaxRefreshConcurrency = defaults.MaxRefreshConcurrency
	}
	if strings.TrimSpace(cfg.QuotaEndpoint) == "" {
		cfg.QuotaEndpoint = defaults.QuotaEndpoint
	}
	if cfg.CircuitFailureThreshold <= 0 {
		cfg.CircuitFailureThreshold = defaults.CircuitFailureThreshold
	}
	if cfg.CircuitOpenDuration <= 0 {
		cfg.CircuitOpenDuration = defaults.CircuitOpenDuration
	}
	if cfg.CircuitHalfOpenSuccessThreshold <= 0 {
		cfg.CircuitHalfOpenSuccessThreshold = defaults.CircuitHalfOpenSuccessThreshold
	}
	if cfg.MaxLogEntries <= 0 {
		cfg.MaxLogEntries = defaults.MaxLogEntries
	}
	if cfg.LogRetention <= 0 {
		cfg.LogRetention = defaults.LogRetention
	}
	return cfg
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
		endpoint, err := validateQuotaEndpoint(decoded.QuotaEndpoint)
		if err != nil {
			return Config{}, err
		}
		cfg.QuotaEndpoint = endpoint
	}
	if decoded.CircuitFailureThreshold != nil {
		if *decoded.CircuitFailureThreshold <= 0 {
			return Config{}, fmt.Errorf("circuit_failure_threshold must be positive")
		}
		cfg.CircuitFailureThreshold = *decoded.CircuitFailureThreshold
	}
	if decoded.CircuitOpenDuration != "" {
		d, err := time.ParseDuration(decoded.CircuitOpenDuration)
		if err != nil {
			return Config{}, fmt.Errorf("circuit_open_duration: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("circuit_open_duration must be positive")
		}
		cfg.CircuitOpenDuration = d
	}
	if decoded.CircuitHalfOpenSuccessThreshold != nil {
		if *decoded.CircuitHalfOpenSuccessThreshold <= 0 {
			return Config{}, fmt.Errorf("circuit_half_open_success_threshold must be positive")
		}
		cfg.CircuitHalfOpenSuccessThreshold = *decoded.CircuitHalfOpenSuccessThreshold
	}
	if decoded.MaxLogEntries != nil {
		if *decoded.MaxLogEntries <= 0 {
			return Config{}, fmt.Errorf("max_log_entries must be positive")
		}
		cfg.MaxLogEntries = *decoded.MaxLogEntries
	}
	if decoded.LogRetention != "" {
		d, err := time.ParseDuration(decoded.LogRetention)
		if err != nil {
			return Config{}, fmt.Errorf("log_retention: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("log_retention must be positive")
		}
		cfg.LogRetention = d
	}
	return NormalizeConfig(cfg), nil
}

func validateQuotaEndpoint(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return "", nil
	}
	if endpoint != chatGPTQuotaEndpoint {
		return "", fmt.Errorf("quota_endpoint must be %s", chatGPTQuotaEndpoint)
	}
	return endpoint, nil
}

func PluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             PluginID,
			Version:          pluginVersion,
			Author:           "Jeffery",
			GitHubRepository: "https://github.com/JefferyZhang2019/cpa-plugin-codex-quota-scheduler",
			Logo:             "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/main/docs/logo.png",
		},
		Capabilities: registrationCapabilities{
			Scheduler:     true,
			UsagePlugin:   true,
			ManagementAPI: true,
		},
	}
}
