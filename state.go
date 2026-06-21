package main

import (
	"sort"
	"sync"
	"time"
)

type PluginState struct {
	mu           sync.RWMutex
	cfg          Config
	accounts     map[string]AccountState
	annotations  AnnotationState
	lastSelected string
	lastReason   string
}

func NewPluginState(cfg Config) *PluginState {
	return &PluginState{
		cfg:         cfg,
		accounts:    make(map[string]AccountState),
		annotations: NormalizeAnnotationState(AnnotationState{}),
	}
}

func (s *PluginState) ReplaceConfig(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
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
	s.accounts[key] = cloneAccountState(account)
}

func (s *PluginState) MarkAccountTemporaryExhausted(authID string, resetAt time.Time, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts["auth:"+authID]
	if !ok {
		account = AccountState{AuthID: authID}
	}
	account.TemporaryExhausted = true
	account.TemporaryResetAt = resetAt
	account.LastError = reason
	s.accounts["auth:"+authID] = account
}

func (s *PluginState) RecordSelection(authID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSelected = authID
	s.lastReason = reason
}

func (s *PluginState) Snapshot(now time.Time) StateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	accounts := make([]AccountState, 0, len(s.accounts))
	for _, account := range s.accounts {
		cloned := cloneAccountState(account)
		cloned.Stale = !cloned.LastSuccessAt.IsZero() && now.Sub(cloned.LastSuccessAt) > s.cfg.StaleAfter
		accounts = append(accounts, cloned)
	}
	sort.Slice(accounts, func(i, j int) bool {
		return accountStateKey(accounts[i]) < accountStateKey(accounts[j])
	})
	accounts = ApplyAnnotations(accounts, s.annotations)

	return StateSnapshot{
		Config:       s.cfg,
		Accounts:     accounts,
		Annotations:  cloneAnnotationState(s.annotations),
		LastSelected: s.lastSelected,
		LastReason:   s.lastReason,
		Now:          now,
	}
}

func accountStateKey(account AccountState) string {
	if key := ResolveAnnotationKey(account); key != "" {
		return key
	}
	return ""
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
