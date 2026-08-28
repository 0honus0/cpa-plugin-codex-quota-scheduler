package main

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	coreQuotaEndpoint      = "https://chatgpt.com/backend-api/wham/usage"
	coreDefault429Ban      = 2 * time.Minute
	coreProbeVerifyDelay   = 3 * time.Second
	coreWorkerPollInterval = 30 * time.Second
	coreRosterSyncInterval = 2 * time.Minute
	coreCredentialRecheck  = 2 * time.Minute
	coreResetAdvanceSlop   = time.Minute
)

type CoreConfig struct {
	HandleEnabled             bool          `json:"handle_enabled"`
	QuotaRefreshInterval      time.Duration `json:"quota_refresh_interval"`
	QuotaStaleAfter           time.Duration `json:"quota_stale_after"`
	EnableResetProbe          bool          `json:"enable_reset_probe"`
	ResetProbeAfterResetDelay time.Duration `json:"reset_probe_after_reset_delay"`
	ResetProbeRetryDelay      time.Duration `json:"reset_probe_retry_delay"`
	AutoBan429                bool          `json:"autoban_429"`
	DisableOn401              bool          `json:"disable_on_401"`
}

type rawCoreConfig struct {
	HandleEnabled             *bool  `yaml:"handle_enabled"`
	QuotaRefreshInterval      string `yaml:"quota_refresh_interval"`
	QuotaStaleAfter           string `yaml:"quota_stale_after"`
	EnableResetProbe          *bool  `yaml:"enable_reset_probe"`
	ResetProbeAfterResetDelay string `yaml:"reset_probe_after_reset_delay"`
	ResetProbeRetryDelay      string `yaml:"reset_probe_retry_delay"`
	AutoBan429                *bool  `yaml:"autoban_429"`
	DisableOn401              *bool  `yaml:"disable_on_401"`
}

func DefaultCoreConfig() CoreConfig {
	return CoreConfig{
		HandleEnabled:             true,
		QuotaRefreshInterval:      30 * time.Minute,
		QuotaStaleAfter:           5 * time.Hour,
		EnableResetProbe:          true,
		ResetProbeAfterResetDelay: 5 * time.Minute,
		ResetProbeRetryDelay:      5 * time.Minute,
		AutoBan429:                true,
		DisableOn401:              true,
	}
}

func NormalizeCoreConfig(cfg CoreConfig) CoreConfig {
	defaults := DefaultCoreConfig()
	if cfg.QuotaRefreshInterval <= 0 {
		cfg.QuotaRefreshInterval = defaults.QuotaRefreshInterval
	}
	if cfg.QuotaStaleAfter <= 0 {
		cfg.QuotaStaleAfter = defaults.QuotaStaleAfter
	}
	if cfg.ResetProbeAfterResetDelay <= 0 {
		cfg.ResetProbeAfterResetDelay = defaults.ResetProbeAfterResetDelay
	}
	if cfg.ResetProbeRetryDelay <= 0 {
		cfg.ResetProbeRetryDelay = defaults.ResetProbeRetryDelay
	}
	return cfg
}

func DecodeCoreConfig(raw []byte) (CoreConfig, error) {
	cfg := DefaultCoreConfig()
	if len(raw) == 0 {
		return cfg, nil
	}
	var decoded rawCoreConfig
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		return CoreConfig{}, err
	}
	if decoded.HandleEnabled != nil {
		cfg.HandleEnabled = *decoded.HandleEnabled
	}
	if decoded.EnableResetProbe != nil {
		cfg.EnableResetProbe = *decoded.EnableResetProbe
	}
	if decoded.AutoBan429 != nil {
		cfg.AutoBan429 = *decoded.AutoBan429
	}
	if decoded.DisableOn401 != nil {
		cfg.DisableOn401 = *decoded.DisableOn401
	}
	var err error
	if decoded.QuotaRefreshInterval != "" {
		cfg.QuotaRefreshInterval, err = positiveCoreDuration("quota_refresh_interval", decoded.QuotaRefreshInterval)
		if err != nil {
			return CoreConfig{}, err
		}
	}
	if decoded.QuotaStaleAfter != "" {
		cfg.QuotaStaleAfter, err = positiveCoreDuration("quota_stale_after", decoded.QuotaStaleAfter)
		if err != nil {
			return CoreConfig{}, err
		}
	}
	if decoded.ResetProbeAfterResetDelay != "" {
		cfg.ResetProbeAfterResetDelay, err = positiveCoreDuration("reset_probe_after_reset_delay", decoded.ResetProbeAfterResetDelay)
		if err != nil {
			return CoreConfig{}, err
		}
	}
	if decoded.ResetProbeRetryDelay != "" {
		cfg.ResetProbeRetryDelay, err = positiveCoreDuration("reset_probe_retry_delay", decoded.ResetProbeRetryDelay)
		if err != nil {
			return CoreConfig{}, err
		}
	}
	return NormalizeCoreConfig(cfg), nil
}

func positiveCoreDuration(name, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return d, nil
}

type CoreSettingsPayload struct {
	HandleEnabled             bool   `json:"handle_enabled"`
	QuotaRefreshInterval      string `json:"quota_refresh_interval"`
	QuotaStaleAfter           string `json:"quota_stale_after"`
	EnableResetProbe          bool   `json:"enable_reset_probe"`
	ResetProbeAfterResetDelay string `json:"reset_probe_after_reset_delay"`
	ResetProbeRetryDelay      string `json:"reset_probe_retry_delay"`
	AutoBan429                bool   `json:"autoban_429"`
	DisableOn401              bool   `json:"disable_on_401"`
}

func coreSettingsFromConfig(cfg CoreConfig) CoreSettingsPayload {
	cfg = NormalizeCoreConfig(cfg)
	return CoreSettingsPayload{
		HandleEnabled:             cfg.HandleEnabled,
		QuotaRefreshInterval:      cfg.QuotaRefreshInterval.String(),
		QuotaStaleAfter:           cfg.QuotaStaleAfter.String(),
		EnableResetProbe:          cfg.EnableResetProbe,
		ResetProbeAfterResetDelay: cfg.ResetProbeAfterResetDelay.String(),
		ResetProbeRetryDelay:      cfg.ResetProbeRetryDelay.String(),
		AutoBan429:                cfg.AutoBan429,
		DisableOn401:              cfg.DisableOn401,
	}
}

func coreConfigFromSettings(base CoreConfig, payload CoreSettingsPayload) (CoreConfig, error) {
	cfg := NormalizeCoreConfig(base)
	cfg.HandleEnabled = payload.HandleEnabled
	cfg.EnableResetProbe = payload.EnableResetProbe
	cfg.AutoBan429 = payload.AutoBan429
	cfg.DisableOn401 = payload.DisableOn401
	var err error
	if payload.QuotaRefreshInterval != "" {
		cfg.QuotaRefreshInterval, err = positiveCoreDuration("quota_refresh_interval", payload.QuotaRefreshInterval)
		if err != nil {
			return CoreConfig{}, err
		}
	}
	if payload.QuotaStaleAfter != "" {
		cfg.QuotaStaleAfter, err = positiveCoreDuration("quota_stale_after", payload.QuotaStaleAfter)
		if err != nil {
			return CoreConfig{}, err
		}
	}
	if payload.ResetProbeAfterResetDelay != "" {
		cfg.ResetProbeAfterResetDelay, err = positiveCoreDuration("reset_probe_after_reset_delay", payload.ResetProbeAfterResetDelay)
		if err != nil {
			return CoreConfig{}, err
		}
	}
	if payload.ResetProbeRetryDelay != "" {
		cfg.ResetProbeRetryDelay, err = positiveCoreDuration("reset_probe_retry_delay", payload.ResetProbeRetryDelay)
		if err != nil {
			return CoreConfig{}, err
		}
	}
	return NormalizeCoreConfig(cfg), nil
}
