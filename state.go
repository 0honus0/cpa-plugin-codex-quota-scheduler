package main

import (
	"sort"
	"sync"
	"time"
)

type PluginState struct {
	mu                  sync.RWMutex
	cfg                 Config
	accounts            map[string]AccountState
	annotations         AnnotationState
	logs                []LogEntry
	lastSelected        string
	lastReason          string
	lastCodexActivityAt time.Time
}

func NewPluginState(cfg Config) *PluginState {
	return &PluginState{
		cfg:         NormalizeConfig(cfg),
		accounts:    make(map[string]AccountState),
		annotations: NormalizeAnnotationState(AnnotationState{}),
	}
}

func (s *PluginState) ReplaceConfig(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = NormalizeConfig(cfg)
}

func (s *PluginState) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *PluginState) SetAnnotations(state AnnotationState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.annotations = cloneAnnotationState(NormalizeAnnotationState(state))
}

func (s *PluginState) Annotations() AnnotationState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneAnnotationState(s.annotations)
}

func (s *PluginState) UpsertQuota(account AccountState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := accountStateKey(account)
	if key == "" {
		return
	}
	if !account.LastSuccessAt.IsZero() && account.LastError == "" {
		account.Refresh = AccountRefreshState{}
	}
	s.accounts[key] = cloneAccountState(account)
}

func (s *PluginState) MarkAccountTemporaryExhausted(authID string, resetAt time.Time, reason string) {
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts["auth:"+authID]
	if !ok {
		account = AccountState{AuthID: authID}
	}
	markTemporaryExhausted(&account, resetAt, reason)
	s.accounts["auth:"+authID] = account
}

func (s *PluginState) MarkAccountTemporaryExhaustedByAuthIndex(authIndex string, resetAt time.Time, reason string) {
	if authIndex == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, account := range s.accounts {
		if account.AuthIndex != authIndex {
			continue
		}
		markTemporaryExhausted(&account, resetAt, reason)
		s.accounts[key] = account
		return
	}
}

func (s *PluginState) RecordAccountFailure(authID, authIndex, reason string, resetAt, now time.Time) (AccountState, bool) {
	if authID == "" && authIndex == "" {
		return AccountState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, account, ok := s.findAccountLocked(authID, authIndex)
	if !ok {
		if authID == "" {
			return AccountState{}, false
		}
		key = "auth:" + authID
		account = AccountState{AuthID: authID, AuthIndex: authIndex, Provider: "codex"}
	}
	applyCircuitFailure(&account, NormalizeConfig(s.cfg), reason, resetAt, now)
	s.accounts[key] = account
	return cloneAccountState(account), true
}

func (s *PluginState) RecordAccountSuccess(authID, authIndex string, now time.Time) (AccountState, bool) {
	if authID == "" && authIndex == "" {
		return AccountState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, account, ok := s.findAccountLocked(authID, authIndex)
	if !ok {
		return AccountState{}, false
	}
	applyCircuitSuccess(&account, NormalizeConfig(s.cfg), now)
	s.accounts[key] = account
	return cloneAccountState(account), true
}

func (s *PluginState) findAccountLocked(authID, authIndex string) (string, AccountState, bool) {
	if authID != "" {
		if account, ok := s.accounts["auth:"+authID]; ok {
			return "auth:" + authID, account, true
		}
	}
	if authIndex != "" {
		for key, account := range s.accounts {
			if account.AuthIndex == authIndex {
				return key, account, true
			}
		}
	}
	return "", AccountState{}, false
}

func (s *PluginState) RecordSelection(authID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSelected = authID
	s.lastReason = reason
}

func (s *PluginState) RecordCodexActivity(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastCodexActivityAt.IsZero() || now.After(s.lastCodexActivityAt) {
		s.lastCodexActivityAt = now
	}
}

func (s *PluginState) RefreshActive(now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := NormalizeConfig(s.cfg)
	return !s.lastCodexActivityAt.IsZero() && !now.After(s.lastCodexActivityAt.Add(cfg.RefreshActiveWindow))
}

func (s *PluginState) RecordRefreshFailure(authID, authIndex string, kind RefreshFailureKind, message string, now time.Time) (AccountState, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	if authID == "" && authIndex == "" {
		return AccountState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key, account, ok := s.findAccountLocked(authID, authIndex)
	if !ok {
		if authID == "" {
			return AccountState{}, false
		}
		key = "auth:" + authID
		account = AccountState{AuthID: authID, AuthIndex: authIndex, Provider: "codex"}
	}
	account.LastError = message
	account.Refresh.LastFailureKind = kind
	account.Refresh.LastFailureAt = now
	if kind == RefreshFailureAuth || kind == RefreshFailureLocal {
		account.Refresh.AuthFailure = kind == RefreshFailureAuth
		account.Refresh.NextRetryAt = time.Time{}
	} else {
		account.Refresh.AuthFailure = false
		account.Refresh.RetryAttempt++
		account.Refresh.NextRetryAt = now.Add(retryDelayForAttempt(NormalizeConfig(s.cfg), account.Refresh.RetryAttempt))
	}
	s.accounts[key] = account
	return cloneAccountState(account), true
}

func (s *PluginState) RecordLog(level, event, message string, fields map[string]any, now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := LogEntry{
		Time:    now,
		Level:   level,
		Event:   event,
		Message: message,
		Fields:  cloneMap(fields),
	}
	s.logs = append(s.logs, entry)
	s.logs = retainedLogs(s.logs, NormalizeConfig(s.cfg), now)
}

func (s *PluginState) Snapshot(now time.Time) StateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	accounts := make([]AccountState, 0, len(s.accounts))
	for _, account := range s.accounts {
		cloned := cloneAccountState(account)
		cloned.Stale = !cloned.LastSuccessAt.IsZero() && now.Sub(cloned.LastSuccessAt) > s.cfg.StaleAfter
		cloned.Circuit = effectiveCircuitState(cloned.Circuit, now)
		accounts = append(accounts, cloned)
	}
	sort.Slice(accounts, func(i, j int) bool {
		return accountStateKey(accounts[i]) < accountStateKey(accounts[j])
	})
	accounts = ApplyAnnotations(accounts, s.annotations)

	return StateSnapshot{
		Config:              s.cfg,
		Accounts:            accounts,
		Annotations:         cloneAnnotationState(s.annotations),
		Logs:                cloneLogs(retainedLogs(s.logs, NormalizeConfig(s.cfg), now)),
		LastSelected:        s.lastSelected,
		LastReason:          s.lastReason,
		LastCodexActivityAt: s.lastCodexActivityAt,
		Now:                 now,
	}
}

func (s *PluginState) DueAccounts(now time.Time) []AccountState {
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := NormalizeConfig(s.cfg)
	accounts := make([]AccountState, 0)
	for _, account := range s.accounts {
		cloned := cloneAccountState(account)
		if due, reason := accountRefreshDue(cloned, cfg, now); due {
			cloned.Refresh.DueReason = reason
			accounts = append(accounts, cloned)
		}
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Priority != accounts[j].Priority {
			return accounts[i].Priority > accounts[j].Priority
		}
		return accountStateKey(accounts[i]) < accountStateKey(accounts[j])
	})
	return accounts
}

func retainedLogs(logs []LogEntry, cfg Config, now time.Time) []LogEntry {
	if len(logs) == 0 {
		return nil
	}
	cfg = NormalizeConfig(cfg)
	cutoff := now.Add(-cfg.LogRetention)
	retained := make([]LogEntry, 0, len(logs))
	for _, log := range logs {
		if log.Time.IsZero() || !log.Time.Before(cutoff) {
			retained = append(retained, log)
		}
	}
	if len(retained) > cfg.MaxLogEntries {
		retained = retained[len(retained)-cfg.MaxLogEntries:]
	}
	return retained
}

func accountRefreshDue(account AccountState, cfg Config, now time.Time) (bool, string) {
	cfg = NormalizeConfig(cfg)
	if account.Refresh.AuthFailure {
		return false, "auth_failure"
	}
	if !account.Refresh.NextRetryAt.IsZero() && account.Refresh.NextRetryAt.After(now) {
		return false, "retry_wait"
	}
	if !account.Refresh.NextRetryAt.IsZero() && !account.Refresh.NextRetryAt.After(now) {
		return true, "retry_due"
	}
	if account.LastSuccessAt.IsZero() {
		return true, "never_refreshed"
	}
	if now.Sub(account.LastSuccessAt) > cfg.StaleAfter {
		return true, "stale"
	}
	if resetDue(account.Quota.FiveHour, cfg.RefreshAfterResetDelay, now) {
		return true, "five_hour_reset_due"
	}
	if resetDue(account.Quota.LongWindow, cfg.RefreshAfterResetDelay, now) {
		return true, "long_window_reset_due"
	}
	if account.TemporaryExhausted && !account.TemporaryResetAt.IsZero() && !account.TemporaryResetAt.Add(cfg.RefreshAfterResetDelay).After(now) {
		return true, "temporary_reset_due"
	}
	return false, ""
}

func resetDue(window *QuotaWindow, delay time.Duration, now time.Time) bool {
	return window != nil && !window.ResetAt.IsZero() && !window.ResetAt.Add(delay).After(now)
}

func retryDelayForAttempt(cfg Config, attempt int) time.Duration {
	delays := NormalizeConfig(cfg).RefreshRetryDelays
	if len(delays) == 0 {
		return time.Minute
	}
	index := attempt - 1
	if index < 0 {
		index = 0
	}
	if index >= len(delays) {
		index = len(delays) - 1
	}
	return delays[index]
}

func accountStateKey(account AccountState) string {
	if key := ResolveAnnotationKey(account); key != "" {
		return key
	}
	return ""
}

func markTemporaryExhausted(account *AccountState, resetAt time.Time, reason string) {
	account.TemporaryExhausted = true
	account.TemporaryResetAt = resetAt
	account.LastError = reason
}

func applyCircuitFailure(account *AccountState, cfg Config, reason string, resetAt, now time.Time) {
	circuit := effectiveCircuitState(account.Circuit, now)
	circuit.FailureCount++
	circuit.SuccessCount = 0
	circuit.Reason = reason
	circuit.LastFailureAt = now
	nextProbeAt := now.Add(cfg.CircuitOpenDuration)
	if !resetAt.IsZero() && resetAt.After(nextProbeAt) {
		nextProbeAt = resetAt
	}
	if !resetAt.IsZero() && resetAt.After(now) && circuit.NextProbeAt.Before(nextProbeAt) {
		circuit.NextProbeAt = nextProbeAt
	}
	threshold := cfg.CircuitFailureThreshold
	if threshold <= 0 {
		threshold = 1
	}
	if circuit.State == CircuitStateHalfOpen || circuit.EffectiveState == CircuitStateHalfOpen || circuit.FailureCount >= threshold {
		circuit.State = CircuitStateOpen
		circuit.EffectiveState = CircuitStateOpen
		circuit.OpenedAt = now
		circuit.NextProbeAt = nextProbeAt
	}
	account.Circuit = circuit
	account.LastError = reason
}

func applyCircuitSuccess(account *AccountState, cfg Config, now time.Time) {
	circuit := effectiveCircuitState(account.Circuit, now)
	if circuit.EffectiveState == CircuitStateHalfOpen {
		circuit.SuccessCount++
		circuit.LastSuccessAt = now
		threshold := cfg.CircuitHalfOpenSuccessThreshold
		if threshold <= 0 {
			threshold = 1
		}
		if circuit.SuccessCount >= threshold {
			account.Circuit = CircuitBreakerState{State: CircuitStateClosed, EffectiveState: CircuitStateClosed}
			account.LastError = ""
			return
		}
		account.Circuit = circuit
		account.LastError = ""
		return
	}
	if circuit.EffectiveState == CircuitStateOpen {
		circuit.LastSuccessAt = now
		account.Circuit = circuit
		return
	}
	account.Circuit = CircuitBreakerState{State: CircuitStateClosed, EffectiveState: CircuitStateClosed, LastSuccessAt: now}
	account.LastError = ""
}

func effectiveCircuitState(circuit CircuitBreakerState, now time.Time) CircuitBreakerState {
	if circuit.State == "" {
		circuit.State = CircuitStateClosed
	}
	circuit.EffectiveState = circuit.State
	if circuit.State == CircuitStateOpen && !circuit.NextProbeAt.IsZero() && !circuit.NextProbeAt.After(now) {
		circuit.EffectiveState = CircuitStateHalfOpen
	}
	return circuit
}

func cloneAnnotationState(state AnnotationState) AnnotationState {
	cloned := AnnotationState{
		Accounts: make(map[string]AccountAnnotation, len(state.Accounts)),
		Groups:   make(map[string]GroupAnnotation, len(state.Groups)),
	}
	for key, annotation := range state.Accounts {
		cloned.Accounts[key] = cloneAccountAnnotation(annotation)
	}
	for key, annotation := range state.Groups {
		cloned.Groups[key] = cloneGroupAnnotation(annotation)
	}
	return cloned
}

func cloneAccountAnnotation(annotation AccountAnnotation) AccountAnnotation {
	annotation.Tags = cloneStringSlice(annotation.Tags)
	return annotation
}

func cloneGroupAnnotation(annotation GroupAnnotation) GroupAnnotation {
	annotation.Tags = cloneStringSlice(annotation.Tags)
	return annotation
}

func cloneAccountState(account AccountState) AccountState {
	account.Quota = cloneParsedQuota(account.Quota)
	account.Annotation = cloneAccountAnnotation(account.Annotation)
	return account
}

func cloneParsedQuota(quota ParsedQuota) ParsedQuota {
	if quota.FiveHour != nil {
		window := *quota.FiveHour
		quota.FiveHour = &window
	}
	if quota.LongWindow != nil {
		window := *quota.LongWindow
		quota.LongWindow = &window
	}
	quota.CodeReviewWindows = cloneQuotaWindows(quota.CodeReviewWindows)
	quota.AdditionalWindows = cloneQuotaWindows(quota.AdditionalWindows)
	if quota.ResetCreditsAvailableCount != nil {
		count := *quota.ResetCreditsAvailableCount
		quota.ResetCreditsAvailableCount = &count
	}
	if quota.ResetCreditsTotalEarnedCount != nil {
		count := *quota.ResetCreditsTotalEarnedCount
		quota.ResetCreditsTotalEarnedCount = &count
	}
	if len(quota.ResetCredits) > 0 {
		quota.ResetCredits = append([]ResetCredit(nil), quota.ResetCredits...)
	}
	return quota
}

func cloneQuotaWindows(windows []QuotaWindow) []QuotaWindow {
	if len(windows) == 0 {
		return nil
	}
	cloned := make([]QuotaWindow, len(windows))
	copy(cloned, windows)
	for i := range cloned {
		if cloned[i].UsedPercent != nil {
			used := *cloned[i].UsedPercent
			cloned[i].UsedPercent = &used
		}
		if cloned[i].LimitWindowSeconds != nil {
			limit := *cloned[i].LimitWindowSeconds
			cloned[i].LimitWindowSeconds = &limit
		}
	}
	return cloned
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneLogs(values []LogEntry) []LogEntry {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]LogEntry, len(values))
	for i, entry := range values {
		cloned[i] = entry
		cloned[i].Fields = cloneMap(entry.Fields)
	}
	return cloned
}

func cloneMap(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
