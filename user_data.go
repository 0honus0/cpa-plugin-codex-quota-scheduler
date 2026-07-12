package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const CurrentUserDataSchema = 1

var ErrUserDataFutureSchema = errors.New("user data schema is newer than supported")

type UserDataRecoveryReport struct{ Migrated, UsedBackup, RecoveredDefault bool }

type SemanticStatePaths struct{ Legacy, UserData, Runtime string }

func semanticStatePaths(legacy string) SemanticStatePaths {
	dir := filepath.Dir(legacy)
	return SemanticStatePaths{Legacy: legacy, UserData: filepath.Join(dir, ".user-data.json"), Runtime: filepath.Join(dir, ".runtime-state.json")}
}

type userDataEnvelope struct {
	SchemaVersion int                          `json:"schema_version"`
	Config        Config                       `json:"config"`
	Accounts      map[string]AccountAnnotation `json:"accounts,omitempty"`
	Groups        map[string]GroupAnnotation   `json:"groups,omitempty"`
}

func loadUserDataWithMigration(paths SemanticStatePaths, hooks FileHooks, crash CrashHitter) (PluginDiskState, bool, error) {
	if _, err := os.Stat(paths.UserData); err == nil {
		return loadUserData(paths.UserData)
	} else if !errors.Is(err, os.ErrNotExist) {
		return PluginDiskState{}, false, err
	}
	legacy, loaded, err := loadPluginDiskState(paths.Legacy)
	if err != nil || !loaded {
		return legacy, loaded, err
	}
	if err := writeUserDataAtomic(paths.UserData, legacy, hooks, crash); err != nil {
		return PluginDiskState{}, false, err
	}
	verified, _, err := loadUserData(paths.UserData)
	if err != nil {
		return PluginDiskState{}, false, err
	}
	//kpoint:K_USER_MIGRATION_AFTER_READBACK
	if crash != nil {
		if err := crash.Hit("K_USER_MIGRATION_AFTER_READBACK"); err != nil {
			return PluginDiskState{}, false, err
		}
	}
	want, _ := json.Marshal(normalizePluginDiskState(legacy))
	got, _ := json.Marshal(verified)
	if string(want) != string(got) {
		return PluginDiskState{}, false, errors.New("migrated user data validation mismatch")
	}
	//kpoint:K_USER_MIGRATION_BEFORE_LEGACY_RENAME
	if crash != nil {
		if err := crash.Hit("K_USER_MIGRATION_BEFORE_LEGACY_RENAME"); err != nil {
			return PluginDiskState{}, false, err
		}
	}
	if err := hooks.Replace(paths.Legacy, paths.Legacy+".migrated"); err != nil {
		return PluginDiskState{}, false, err
	}
	//kpoint:K_USER_MIGRATION_AFTER_LEGACY_RENAME
	if crash != nil {
		if err := crash.Hit("K_USER_MIGRATION_AFTER_LEGACY_RENAME"); err != nil {
			return PluginDiskState{}, false, err
		}
	}
	_ = hooks.SyncDir(filepath.Dir(paths.Legacy))
	return verified, true, nil
}

func loadUserData(path string) (PluginDiskState, bool, error) {
	state, loaded, _, err := loadUserDataRecover(path)
	return state, loaded, err
}

func loadUserDataRecover(path string) (PluginDiskState, bool, UserDataRecoveryReport, error) {
	report := UserDataRecoveryReport{}
	state, loaded, err := readUserData(path)
	if err == nil {
		raw, _ := os.ReadFile(path)
		var env userDataEnvelope
		_ = json.Unmarshal(raw, &env)
		if loaded && env.SchemaVersion < CurrentUserDataSchema {
			if werr := SaveUserData(path, state); werr != nil {
				return PluginDiskState{}, false, report, werr
			}
			report.Migrated = true
		}
		return state, loaded, report, nil
	}
	if errors.Is(err, ErrUserDataFutureSchema) {
		return PluginDiskState{}, false, report, err
	}
	backup, backupLoaded, backupErr := readUserData(path + ".bak")
	if backupErr == nil {
		if !errors.Is(err, os.ErrNotExist) {
			if qerr := quarantineUserArtifact(path); qerr != nil {
				return PluginDiskState{}, false, report, qerr
			}
		}
		if werr := SaveUserData(path, backup); werr != nil {
			return PluginDiskState{}, false, report, werr
		}
		report.UsedBackup = true
		return backup, backupLoaded, report, nil
	}
	if errors.Is(err, os.ErrNotExist) && errors.Is(backupErr, os.ErrNotExist) {
		return normalizePluginDiskState(PluginDiskState{Config: DefaultConfig()}), false, report, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		if qerr := quarantineUserArtifact(path); qerr != nil {
			return PluginDiskState{}, false, report, qerr
		}
	}
	if !errors.Is(backupErr, os.ErrNotExist) {
		if qerr := quarantineUserArtifact(path + ".bak"); qerr != nil {
			return PluginDiskState{}, false, report, qerr
		}
	}
	report.RecoveredDefault = true
	return normalizePluginDiskState(PluginDiskState{Config: DefaultConfig()}), false, report, nil
}

func quarantineUserArtifact(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	target := path + ".corrupt"
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("corrupt evidence already exists: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := replaceFileAtomic(path, target); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func readUserData(path string) (PluginDiskState, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return normalizePluginDiskState(PluginDiskState{Config: DefaultConfig()}), false, nil
	}
	if err != nil {
		return PluginDiskState{}, false, err
	}
	var env userDataEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return PluginDiskState{}, false, err
	}
	if env.SchemaVersion > CurrentUserDataSchema {
		return PluginDiskState{}, false, fmt.Errorf("%w: %d", ErrUserDataFutureSchema, env.SchemaVersion)
	}
	return normalizePluginDiskState(PluginDiskState{Config: env.Config, Accounts: env.Accounts, Groups: env.Groups}), true, nil
}

func writeUserDataAtomic(path string, state PluginDiskState, hooks FileHooks, crash CrashHitter) error {
	defaults := OSFileHooks()
	if hooks.Replace == nil {
		hooks.Replace = defaults.Replace
	}
	if hooks.SyncDir == nil {
		hooks.SyncDir = defaults.SyncDir
	}
	if hooks.ReadFile == nil {
		hooks.ReadFile = defaults.ReadFile
	}
	state = normalizePluginDiskState(state)
	env := userDataEnvelope{CurrentUserDataSchema, state.Config, state.Accounts, state.Groups}
	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	current, readErr := hooks.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		current = raw
	} else if readErr != nil {
		return readErr
	}
	if err := writeUserArtifactAtomic(path+".bak", current, hooks, crash); err != nil {
		return err
	}
	tmp := path + ".tmp"
	//kpoint:K_USER_PRIMARY_TEMP_WRITE
	if crash != nil {
		if err := crash.Hit("K_USER_PRIMARY_TEMP_WRITE"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	//kpoint:K_USER_PRIMARY_TEMP_FSYNC
	if crash != nil {
		if err := crash.Hit("K_USER_PRIMARY_TEMP_FSYNC"); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	//kpoint:K_USER_MIGRATION_BEFORE_NEW_RENAME
	if crash != nil {
		if err := crash.Hit("K_USER_MIGRATION_BEFORE_NEW_RENAME"); err != nil {
			return err
		}
	}
	if err := hooks.Replace(tmp, path); err != nil {
		return err
	}
	//kpoint:K_USER_MIGRATION_AFTER_NEW_RENAME
	if crash != nil {
		if err := crash.Hit("K_USER_MIGRATION_AFTER_NEW_RENAME"); err != nil {
			return err
		}
	}
	//kpoint:K_USER_PRIMARY_DIR_FSYNC
	if crash != nil {
		if err := crash.Hit("K_USER_PRIMARY_DIR_FSYNC"); err != nil {
			return err
		}
	}
	if err := hooks.SyncDir(filepath.Dir(path)); err != nil {
		return err
	}
	//kpoint:K_USER_AFTER_PRIMARY_DIR_FSYNC
	if crash != nil {
		if err := crash.Hit("K_USER_AFTER_PRIMARY_DIR_FSYNC"); err != nil {
			return err
		}
	}
	return nil
}

func writeUserArtifactAtomic(path string, raw []byte, hooks FileHooks, crash CrashHitter) error {
	tmp := path + ".tmp"
	//kpoint:K_USER_BACKUP_TEMP_WRITE
	if crash != nil {
		if err := crash.Hit("K_USER_BACKUP_TEMP_WRITE"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	//kpoint:K_USER_BACKUP_TEMP_FSYNC
	if crash != nil {
		if err := crash.Hit("K_USER_BACKUP_TEMP_FSYNC"); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	//kpoint:K_USER_BACKUP_BEFORE_REPLACE
	if crash != nil {
		if err := crash.Hit("K_USER_BACKUP_BEFORE_REPLACE"); err != nil {
			return err
		}
	}
	if err = hooks.Replace(tmp, path); err != nil {
		return err
	}
	//kpoint:K_USER_BACKUP_AFTER_REPLACE
	if crash != nil {
		if err := crash.Hit("K_USER_BACKUP_AFTER_REPLACE"); err != nil {
			return err
		}
	}
	//kpoint:K_USER_BACKUP_DIR_FSYNC
	if crash != nil {
		if err := crash.Hit("K_USER_BACKUP_DIR_FSYNC"); err != nil {
			return err
		}
	}
	if err := hooks.SyncDir(filepath.Dir(path)); err != nil {
		return err
	}
	//kpoint:K_USER_AFTER_BACKUP_DIR_FSYNC
	if crash != nil {
		if err := crash.Hit("K_USER_AFTER_BACKUP_DIR_FSYNC"); err != nil {
			return err
		}
	}
	return nil
}

func SaveUserData(path string, state PluginDiskState) error {
	return writeUserDataAtomic(path, state, OSFileHooks(), nil)
}
