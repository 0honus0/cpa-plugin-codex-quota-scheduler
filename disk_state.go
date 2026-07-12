package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type PluginDiskState struct {
	Config   Config                       `json:"config"`
	Accounts map[string]AccountAnnotation `json:"accounts,omitempty"`
	Groups   map[string]GroupAnnotation   `json:"groups,omitempty"`
}

// PersistentState is deliberately separate from PluginDiskState: the former
// contains only scheduler-control hashes and epochs, never host auth material.

var defaultStatePath = resolveDefaultStatePath

func resolveDefaultStatePath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "CLIProxyAPI", PluginID, "state.json")
}

func LoadPluginDiskState(path string) (PluginDiskState, error) {
	state, _, err := loadPluginDiskState(path)
	return state, err
}

func loadPluginDiskState(path string) (PluginDiskState, bool, error) {
	state := PluginDiskState{Config: DefaultConfig()}
	if path == "" {
		return normalizePluginDiskState(state), false, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return normalizePluginDiskState(state), false, nil
	}
	if err != nil {
		return PluginDiskState{}, false, err
	}
	if len(raw) == 0 {
		return normalizePluginDiskState(state), false, nil
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return PluginDiskState{}, false, err
	}
	return normalizePluginDiskState(state), true, nil
}

func SavePluginDiskState(path string, state PluginDiskState) error {
	if path == "" {
		path = defaultStatePath()
	}
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(normalizePluginDiskState(state), "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0600)
}

func normalizePluginDiskState(state PluginDiskState) PluginDiskState {
	cfg := state.Config
	if cfg.QuotaRefreshInterval <= 0 && cfg.StaleAfter <= 0 && cfg.MonthlyMode == "" {
		cfg = DefaultConfig()
	}
	cfg = NormalizeConfig(cfg)
	if _, err := validateQuotaEndpoint(cfg.QuotaEndpoint); err != nil {
		cfg.QuotaEndpoint = DefaultConfig().QuotaEndpoint
	}
	annotations := NormalizeAnnotationState(AnnotationState{
		Accounts: state.Accounts,
		Groups:   state.Groups,
	})
	state.Config = cfg
	state.Accounts = annotations.Accounts
	state.Groups = annotations.Groups
	return state
}

func diskStateFromStore(store *PluginState) PluginDiskState {
	if store == nil {
		return PluginDiskState{Config: DefaultConfig()}
	}
	annotations := store.Annotations()
	return PluginDiskState{
		Config:   store.Config(),
		Accounts: annotations.Accounts,
		Groups:   annotations.Groups,
	}
}
