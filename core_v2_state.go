package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type CoreAccountPreference struct {
	Alias             string `json:"alias,omitempty"`
	SchedulerPriority int    `json:"scheduler_priority,omitempty"`
	RefreshEnabled    *bool  `json:"refresh_enabled,omitempty"`
}

func (p CoreAccountPreference) IsRefreshEnabled() bool {
	return p.RefreshEnabled == nil || *p.RefreshEnabled
}

type CoreAccount struct {
	ID                    string                `json:"auth_id"`
	AuthIndex             string                `json:"auth_index,omitempty"`
	Name                  string                `json:"name,omitempty"`
	Label                 string                `json:"label,omitempty"`
	Email                 string                `json:"email,omitempty"`
	Priority              int                   `json:"cpa_priority"`
	HostStatus            string                `json:"host_status,omitempty"`
	Preference            CoreAccountPreference `json:"preference"`
	Disabled401           bool                  `json:"disabled_401"`
	Disabled401At         time.Time             `json:"disabled_401_at,omitempty"`
	DisabledFingerprint   string                `json:"-"`
	BanUntil              time.Time             `json:"ban_until,omitempty"`
	BanReason             string                `json:"ban_reason,omitempty"`
	Quota                 ParsedQuota           `json:"quota"`
	LastRefreshAt         time.Time             `json:"last_refresh_at,omitempty"`
	LastSuccessAt         time.Time             `json:"last_success_at,omitempty"`
	LastError             string                `json:"last_error,omitempty"`
	CredentialFingerprint string                `json:"-"`
	LastCredentialCheckAt time.Time             `json:"last_credential_check_at,omitempty"`
	ProbeBaselineResetAt  time.Time             `json:"probe_baseline_reset_at,omitempty"`
	ProbeDueAt            time.Time             `json:"probe_due_at,omitempty"`
	LastProbeAt           time.Time             `json:"last_probe_at,omitempty"`
	ProbeStatus           string                `json:"probe_status,omitempty"`
}

func (a CoreAccount) RefreshEnabled() bool { return a.Preference.IsRefreshEnabled() }

func (a CoreAccount) Banned(now time.Time) bool {
	return !a.BanUntil.IsZero() && now.Before(a.BanUntil)
}

func (a CoreAccount) Selectable(now time.Time, _ time.Duration) bool {
	// Hard availability is deliberately limited to request-result safety state.
	// Quota snapshots can be stale or lazy-reset, so they are ordering signals,
	// not permission to override CPA's cross-priority availability decisions.
	return !a.Disabled401 && !a.Banned(now)
}

func coreWindowExhausted(window *QuotaWindow, now time.Time) bool {
	if window == nil || !window.Exhausted {
		return false
	}
	return window.ResetAt.IsZero() || now.Before(window.ResetAt)
}

type CoreLog struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Event   string         `json:"event"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type corePersistedAccount struct {
	Preference           CoreAccountPreference `json:"preference"`
	Disabled401          bool                  `json:"disabled_401,omitempty"`
	Disabled401At        time.Time             `json:"disabled_401_at,omitempty"`
	DisabledFingerprint  string                `json:"disabled_fingerprint,omitempty"`
	BanUntil             time.Time             `json:"ban_until,omitempty"`
	BanReason            string                `json:"ban_reason,omitempty"`
	Quota                ParsedQuota           `json:"quota,omitempty"`
	LastRefreshAt        time.Time             `json:"last_refresh_at,omitempty"`
	LastSuccessAt        time.Time             `json:"last_success_at,omitempty"`
	LastError            string                `json:"last_error,omitempty"`
	ProbeBaselineResetAt time.Time             `json:"probe_baseline_reset_at,omitempty"`
	ProbeDueAt           time.Time             `json:"probe_due_at,omitempty"`
	LastProbeAt          time.Time             `json:"last_probe_at,omitempty"`
	ProbeStatus          string                `json:"probe_status,omitempty"`
}

type corePersistedState struct {
	SchemaVersion  int                             `json:"schema_version"`
	Config         CoreConfig                      `json:"config"`
	ConfigYAMLHash string                          `json:"config_yaml_hash,omitempty"`
	Accounts       map[string]corePersistedAccount `json:"accounts,omitempty"`
}

const corePersistedSchema = 1

type CoreEngine struct {
	host      HostClient
	now       func() time.Time
	statePath string

	mu             sync.RWMutex
	cfg            CoreConfig
	accounts       map[string]*CoreAccount
	persisted      map[string]corePersistedAccount
	logs           []CoreLog
	lastRosterSync time.Time
	lastSelected   string
	lastReason     string
	configYAMLHash string
	hasSavedConfig bool

	workerMu sync.Mutex
	started  bool
	stop     chan struct{}
	wake     chan struct{}
	wg       sync.WaitGroup
	forceMu  sync.Mutex
	forceAll bool
	forced   map[string]struct{}
}

func NewCoreEngine(host HostClient, statePath string, now func() time.Time) *CoreEngine {
	if now == nil {
		now = time.Now
	}
	if statePath == "" {
		statePath = defaultCoreStatePath()
	}
	e := &CoreEngine{
		host:      host,
		now:       now,
		statePath: statePath,
		cfg:       DefaultCoreConfig(),
		accounts:  make(map[string]*CoreAccount),
		persisted: make(map[string]corePersistedAccount),
		stop:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
		forced:    make(map[string]struct{}),
	}
	e.loadPersisted()
	return e
}

func defaultCoreStatePath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "CLIProxyAPI", PluginID, "cron.json")
}

func (e *CoreEngine) loadPersisted() {
	raw, err := os.ReadFile(e.statePath)
	if err != nil {
		return
	}
	var disk corePersistedState
	if json.Unmarshal(raw, &disk) != nil || disk.SchemaVersion != corePersistedSchema {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if disk.Config.QuotaRefreshInterval > 0 {
		e.cfg = NormalizeCoreConfig(disk.Config)
		e.hasSavedConfig = true
	}
	e.configYAMLHash = disk.ConfigYAMLHash
	if disk.Accounts != nil {
		e.persisted = disk.Accounts
	}
}

func (e *CoreEngine) persistLocked() error {
	disk := corePersistedState{SchemaVersion: corePersistedSchema, Config: e.cfg, ConfigYAMLHash: e.configYAMLHash, Accounts: make(map[string]corePersistedAccount, len(e.persisted)+len(e.accounts))}
	for id, p := range e.persisted {
		disk.Accounts[id] = p
	}
	for id, a := range e.accounts {
		disk.Accounts[id] = corePersistedFromAccount(*a)
	}
	raw, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(e.statePath), 0700); err != nil {
		return err
	}
	tmp := e.statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, e.statePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func corePersistedFromAccount(a CoreAccount) corePersistedAccount {
	return corePersistedAccount{
		Preference:           a.Preference,
		Disabled401:          a.Disabled401,
		Disabled401At:        a.Disabled401At,
		DisabledFingerprint:  a.DisabledFingerprint,
		BanUntil:             a.BanUntil,
		BanReason:            a.BanReason,
		Quota:                cloneCoreQuota(a.Quota),
		LastRefreshAt:        a.LastRefreshAt,
		LastSuccessAt:        a.LastSuccessAt,
		LastError:            a.LastError,
		ProbeBaselineResetAt: a.ProbeBaselineResetAt,
		ProbeDueAt:           a.ProbeDueAt,
		LastProbeAt:          a.LastProbeAt,
		ProbeStatus:          a.ProbeStatus,
	}
}

func applyCorePersisted(a *CoreAccount, p corePersistedAccount) {
	a.Preference = p.Preference
	a.Disabled401 = p.Disabled401
	a.Disabled401At = p.Disabled401At
	a.DisabledFingerprint = p.DisabledFingerprint
	a.BanUntil = p.BanUntil
	a.BanReason = p.BanReason
	a.Quota = cloneCoreQuota(p.Quota)
	a.LastRefreshAt = p.LastRefreshAt
	a.LastSuccessAt = p.LastSuccessAt
	a.LastError = p.LastError
	a.ProbeBaselineResetAt = p.ProbeBaselineResetAt
	a.ProbeDueAt = p.ProbeDueAt
	a.LastProbeAt = p.LastProbeAt
	a.ProbeStatus = p.ProbeStatus
}

func cloneCoreQuota(in ParsedQuota) ParsedQuota {
	out := in
	if in.FiveHour != nil {
		w := *in.FiveHour
		if in.FiveHour.UsedPercent != nil {
			v := *in.FiveHour.UsedPercent
			w.UsedPercent = &v
		}
		if in.FiveHour.LimitWindowSeconds != nil {
			v := *in.FiveHour.LimitWindowSeconds
			w.LimitWindowSeconds = &v
		}
		out.FiveHour = &w
	}
	if in.LongWindow != nil {
		w := *in.LongWindow
		if in.LongWindow.UsedPercent != nil {
			v := *in.LongWindow.UsedPercent
			w.UsedPercent = &v
		}
		if in.LongWindow.LimitWindowSeconds != nil {
			v := *in.LongWindow.LimitWindowSeconds
			w.LimitWindowSeconds = &v
		}
		out.LongWindow = &w
	}
	out.ResetCredits = append([]ResetCredit(nil), in.ResetCredits...)
	out.CodeReviewWindows = append([]QuotaWindow(nil), in.CodeReviewWindows...)
	out.AdditionalWindows = append([]QuotaWindow(nil), in.AdditionalWindows...)
	if in.ResetCreditsAvailableCount != nil {
		v := *in.ResetCreditsAvailableCount
		out.ResetCreditsAvailableCount = &v
	}
	if in.ResetCreditsTotalEarnedCount != nil {
		v := *in.ResetCreditsTotalEarnedCount
		out.ResetCreditsTotalEarnedCount = &v
	}
	return out
}

func coreCredentialFingerprint(credentials CodexCredentials) string {
	h := sha256.New()
	_, _ = h.Write([]byte(credentials.ChatGPTAccountID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(credentials.RefreshToken))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(credentials.AccessToken))
	return hex.EncodeToString(h.Sum(nil))
}

func (e *CoreEngine) Config() CoreConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

func (e *CoreEngine) ConfigureOnRegister(raw []byte) error {
	return e.configureHostYAML(raw, false)
}

func (e *CoreEngine) Configure(raw []byte) error {
	return e.configureHostYAML(raw, true)
}

func (e *CoreEngine) configureHostYAML(raw []byte, force bool) error {
	cfg, err := DecodeCoreConfig(raw)
	if err != nil {
		return err
	}
	h := sha256.Sum256(raw)
	hash := hex.EncodeToString(h[:])
	e.mu.Lock()
	preserveSaved := !force && e.hasSavedConfig && (e.configYAMLHash == "" || e.configYAMLHash == hash)
	if !preserveSaved {
		e.setConfigLocked(cfg)
	}
	e.configYAMLHash = hash
	e.hasSavedConfig = true
	err = e.persistLocked()
	e.mu.Unlock()
	if err == nil {
		e.Wake()
	}
	return err
}

func (e *CoreEngine) UpdateConfig(cfg CoreConfig) error {
	e.mu.Lock()
	e.setConfigLocked(cfg)
	e.hasSavedConfig = true
	err := e.persistLocked()
	e.mu.Unlock()
	if err == nil {
		e.Wake()
	}
	return err
}

func (e *CoreEngine) setConfigLocked(cfg CoreConfig) {
	previous := e.cfg
	next := NormalizeCoreConfig(cfg)
	e.cfg = next
	now := e.now()

	// Reconcile persisted accounts too. During startup the live roster is still
	// empty, so only touching e.accounts would leave cron.json on an old delay.
	for id, persisted := range e.persisted {
		account := &CoreAccount{
			Disabled401:          persisted.Disabled401,
			Quota:                cloneCoreQuota(persisted.Quota),
			ProbeBaselineResetAt: persisted.ProbeBaselineResetAt,
			ProbeDueAt:           persisted.ProbeDueAt,
			ProbeStatus:          persisted.ProbeStatus,
		}
		coreReconcileProbeConfig(account, previous, next, now)
		persisted.ProbeBaselineResetAt = account.ProbeBaselineResetAt
		persisted.ProbeDueAt = account.ProbeDueAt
		persisted.ProbeStatus = account.ProbeStatus
		e.persisted[id] = persisted
	}
	for id, account := range e.accounts {
		coreReconcileProbeConfig(account, previous, next, now)
		e.persisted[id] = corePersistedFromAccount(*account)
	}
}

func coreReconcileProbeConfig(account *CoreAccount, previous, next CoreConfig, now time.Time) {
	if account == nil {
		return
	}
	if !next.EnableResetProbe {
		account.ProbeBaselineResetAt = time.Time{}
		account.ProbeDueAt = time.Time{}
		if !account.Disabled401 {
			account.ProbeStatus = ""
		}
		return
	}
	if previous.EnableResetProbe && previous.ResetProbeAfterResetDelay == next.ResetProbeAfterResetDelay {
		return
	}
	if account.Disabled401 || account.Quota.FiveHour == nil || account.Quota.FiveHour.ResetAt.IsZero() {
		return
	}
	canonical := !previous.EnableResetProbe || account.ProbeDueAt.IsZero() || account.ProbeStatus == "" || account.ProbeStatus == "scheduled_after_reset" || account.ProbeStatus == "waiting_after_reset" || account.ProbeStatus == "natural_reset_confirmed"
	if !canonical {
		// Retry/in-flight due times have stronger semantics than the canonical
		// reset+delay schedule, so settings changes must not shorten them.
		return
	}
	resetAt := account.Quota.FiveHour.ResetAt
	account.ProbeBaselineResetAt = resetAt
	account.ProbeDueAt = resetAt.Add(next.ResetProbeAfterResetDelay)
	if resetAt.After(now) {
		account.ProbeStatus = "scheduled_after_reset"
	} else {
		account.ProbeStatus = "waiting_after_reset"
	}
}

func (e *CoreEngine) Accounts() []CoreAccount {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]CoreAccount, 0, len(e.accounts))
	for _, a := range e.accounts {
		copy := *a
		copy.Quota = cloneCoreQuota(a.Quota)
		out = append(out, copy)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		if out[i].Preference.SchedulerPriority != out[j].Preference.SchedulerPriority {
			return out[i].Preference.SchedulerPriority > out[j].Preference.SchedulerPriority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (e *CoreEngine) Logs() []CoreLog {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]CoreLog, len(e.logs))
	copy(out, e.logs)
	return out
}

func (e *CoreEngine) recordLog(level, event, message string, fields map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := CoreLog{Time: e.now(), Level: level, Event: event, Message: message, Fields: fields}
	e.logs = append(e.logs, entry)
	if len(e.logs) > 300 {
		e.logs = append([]CoreLog(nil), e.logs[len(e.logs)-300:]...)
	}
}

func (e *CoreEngine) SetAccountPreference(authID string, refreshEnabled *bool, schedulerPriority *int, alias *string) error {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return errors.New("auth_id is required")
	}
	e.mu.Lock()
	if e.accounts[authID] == nil {
		e.mu.Unlock()
		return errors.New("account not found")
	}
	p := e.persisted[authID].Preference
	if a := e.accounts[authID]; a != nil {
		p = a.Preference
	}
	if refreshEnabled != nil {
		v := *refreshEnabled
		p.RefreshEnabled = &v
	}
	if schedulerPriority != nil {
		p.SchedulerPriority = *schedulerPriority
	}
	if alias != nil {
		p.Alias = strings.TrimSpace(*alias)
	}
	persisted := e.persisted[authID]
	persisted.Preference = p
	e.persisted[authID] = persisted
	if a := e.accounts[authID]; a != nil {
		a.Preference = p
	}
	err := e.persistLocked()
	e.mu.Unlock()
	if err == nil {
		e.Wake()
	}
	return err
}

func (e *CoreEngine) accountByID(authID string) (CoreAccount, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	a := e.accounts[authID]
	if a == nil {
		return CoreAccount{}, false
	}
	copy := *a
	copy.Quota = cloneCoreQuota(a.Quota)
	return copy, true
}

func (e *CoreEngine) clearExpiredBanLocked(a *CoreAccount, now time.Time) {
	if a != nil && !a.BanUntil.IsZero() && !now.Before(a.BanUntil) {
		a.BanUntil = time.Time{}
		a.BanReason = ""
	}
}
