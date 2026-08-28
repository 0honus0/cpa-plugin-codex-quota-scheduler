package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (e *CoreEngine) Start() {
	e.workerMu.Lock()
	if e.started {
		e.workerMu.Unlock()
		return
	}
	e.started = true
	e.stop = make(chan struct{})
	e.wake = make(chan struct{}, 1)
	e.wg.Add(1)
	e.workerMu.Unlock()
	go e.worker()
	e.Wake()
}

func (e *CoreEngine) Stop() {
	e.workerMu.Lock()
	if !e.started {
		e.workerMu.Unlock()
		return
	}
	close(e.stop)
	e.started = false
	e.workerMu.Unlock()
	e.wg.Wait()
}

func (e *CoreEngine) Wake() {
	e.workerMu.Lock()
	if !e.started {
		e.workerMu.Unlock()
		return
	}
	wake := e.wake
	e.workerMu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (e *CoreEngine) RequestRefreshAll() {
	e.forceMu.Lock()
	e.forceAll = true
	e.forceMu.Unlock()
	e.Wake()
}

func (e *CoreEngine) RequestRefreshOne(authID string) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	e.forceMu.Lock()
	e.forced[authID] = struct{}{}
	e.forceMu.Unlock()
	e.Wake()
}

func (e *CoreEngine) consumeForcedRefreshes() (bool, map[string]struct{}) {
	e.forceMu.Lock()
	defer e.forceMu.Unlock()
	all := e.forceAll
	forced := e.forced
	e.forceAll = false
	e.forced = make(map[string]struct{})
	return all, forced
}

func (e *CoreEngine) worker() {
	defer e.wg.Done()
	ticker := time.NewTicker(coreWorkerPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
		case <-e.wake:
		}
		e.RunCycle()
	}
}

func (e *CoreEngine) RunCycle() {
	now := e.now()
	e.mu.RLock()
	lastRosterSync := e.lastRosterSync
	e.mu.RUnlock()
	if lastRosterSync.IsZero() || now.Sub(lastRosterSync) >= coreRosterSyncInterval {
		if err := e.SyncRoster(); err != nil {
			e.recordLog("warn", "roster.sync_failed", "failed to sync enabled Codex accounts", map[string]any{"error": sanitizeCoreError(err)})
			return
		}
	}

	cfg := e.Config()
	forceAll, forced := e.consumeForcedRefreshes()
	for _, account := range e.Accounts() {
		_, forceOne := forced[account.ID]
		forcedRefresh := forceAll || forceOne

		// 401 recovery is a credential-file check, not quota maintenance. Keep it
		// alive even when automatic quota refresh is disabled, and let a manual
		// refresh force an immediate credential recheck.
		if account.Disabled401 {
			if forcedRefresh || account.LastCredentialCheckAt.IsZero() || now.Sub(account.LastCredentialCheckAt) >= coreCredentialRecheck {
				e.recheckCredentialOnly(account)
			}
			updated, ok := e.accountByID(account.ID)
			if !ok || updated.Disabled401 {
				continue
			}
			account = updated
		}

		autoRefresh := account.RefreshEnabled()
		if !autoRefresh && !forcedRefresh {
			continue
		}

		// A business 429 ban is a scheduler-selection guard, not a maintenance
		// lock. Quota reads and reset probes must keep running so a lazy 5h reset
		// can be detected and activated at its configured reset+delay time.
		dueProbe := autoRefresh && cfg.EnableResetProbe && !account.ProbeDueAt.IsZero() && !account.ProbeDueAt.After(now)
		if dueProbe {
			e.recordLog("info", "quota.maintenance_triggered", "reset maintenance triggered", map[string]any{"auth_id": account.ID, "source": "probe_due", "probe_due_at": account.ProbeDueAt})
			// ProbeAccount performs the required read-only quota precheck itself and
			// never POSTs when that precheck fails or already shows a new window.
			_ = e.ProbeAccount(account.ID)
			continue
		}
		dueRefresh := forcedRefresh || (autoRefresh && (account.LastRefreshAt.IsZero() || now.Sub(account.LastRefreshAt) >= cfg.QuotaRefreshInterval))
		if dueRefresh {
			source := "periodic"
			if forcedRefresh {
				source = "manual"
			}
			e.recordLog("info", "quota.refresh_triggered", "quota refresh triggered", map[string]any{"auth_id": account.ID, "source": source})
			_ = e.refreshAccount(account.ID, !forcedRefresh)
		}
	}
}

func (e *CoreEngine) recheckCredentialOnly(account CoreAccount) {
	_, fingerprint, err := e.readCredentials(account)
	if err != nil {
		return
	}
	e.observeCredential(account.ID, fingerprint)
}

func (e *CoreEngine) SyncRoster() error {
	if e.host == nil {
		return errors.New("host unavailable")
	}
	files, err := e.host.ListAuths()
	if err != nil {
		return err
	}
	now := e.now()
	next := make(map[string]*CoreAccount)
	for _, file := range files {
		if !strings.EqualFold(strings.TrimSpace(file.Provider), "codex") || strings.TrimSpace(file.ID) == "" || strings.TrimSpace(file.AuthIndex) == "" {
			continue
		}
		e.mu.RLock()
		persisted, persistedOK := e.persisted[file.ID]
		existing := e.accounts[file.ID]
		e.mu.RUnlock()
		// Operator-disabled accounts are outside scheduler scope. Accounts disabled
		// by this plugin after a business 401 remain visible so credential recovery
		// can be detected and the CPA disabled bit can be safely cleared.
		if file.Disabled && !(persistedOK && persisted.Disabled401) && !(existing != nil && existing.Disabled401) {
			continue
		}
		account := &CoreAccount{
			ID:         file.ID,
			AuthIndex:  file.AuthIndex,
			Name:       file.Name,
			Label:      file.Label,
			Email:      file.Email,
			Priority:   file.Priority,
			HostStatus: file.Status,
		}
		if existing != nil {
			copy := *existing
			account = &copy
			account.AuthIndex = file.AuthIndex
			account.Name = file.Name
			account.Label = file.Label
			account.Email = file.Email
			account.Priority = file.Priority
			account.HostStatus = file.Status
		} else if persistedOK {
			applyCorePersisted(account, persisted)
		}
		next[file.ID] = account
	}

	e.mu.Lock()
	e.accounts = next
	e.lastRosterSync = now
	for id, account := range next {
		// Rehydrate the next reset maintenance timer immediately from persisted
		// quota data. A process restart must not leave a known reset without a
		// probe_due_at until the next periodic network refresh.
		e.updateProbeScheduleLocked(account, account.Quota, account.Quota, now)
		e.persisted[id] = corePersistedFromAccount(*account)
	}
	err = e.persistLocked()
	e.mu.Unlock()
	if err == nil {
		e.recordLog("info", "roster.synced", "synced all enabled Codex accounts", map[string]any{"count": len(next)})
	}
	return err
}

func (e *CoreEngine) RefreshAccount(authID string) error {
	// Manual refresh bypasses the automatic-maintenance switch.
	return e.refreshAccount(authID, false)
}

func (e *CoreEngine) refreshAccount(authID string, requireAutoEnabled bool) error {
	account, ok := e.accountByID(authID)
	if !ok {
		return errors.New("account not found")
	}
	if requireAutoEnabled && !account.RefreshEnabled() {
		return nil
	}
	credentials, fingerprint, err := e.readCredentials(account)
	if err != nil {
		e.finishRefreshError(authID, "credential_read_failed", err)
		return err
	}
	if e.observeCredential(authID, fingerprint) {
		account, _ = e.accountByID(authID)
	}
	if account.Disabled401 {
		return nil
	}

	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    coreQuotaEndpoint,
		Headers: http.Header{
			"Authorization":      []string{"Bearer " + credentials.AccessToken},
			"Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID},
			"Content-Type":       []string{"application/json"},
			"User-Agent":         []string{quotaUserAgent},
		},
	}
	resp, err := e.host.Do(req)
	if err != nil {
		e.finishRefreshError(authID, "quota_transport_failed", err)
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		err = errors.New("quota refresh returned 401")
		e.finishRefreshError(authID, "quota_refresh_401", err)
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		err = errors.New("quota refresh returned 429")
		e.finishRefreshError(authID, "quota_refresh_429", err)
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = errors.New("quota refresh returned HTTP " + strconv.Itoa(resp.StatusCode))
		e.finishRefreshError(authID, "quota_http_failed", err)
		return err
	}
	quota, err := ParseCodexUsagePayload(resp.Body, e.now())
	if err != nil {
		e.finishRefreshError(authID, "quota_decode_failed", err)
		return err
	}
	e.finishRefreshSuccess(authID, quota)
	return nil
}

func (e *CoreEngine) readCredentials(account CoreAccount) (CodexCredentials, string, error) {
	resp, err := e.host.GetAuth(account.AuthIndex)
	if err != nil {
		return CodexCredentials{}, "", err
	}
	credentials, err := ExtractCodexCredentials(resp.JSON)
	if err != nil {
		return CodexCredentials{}, "", err
	}
	return credentials, coreCredentialFingerprint(credentials), nil
}

// observeCredential returns true when the live account state changed. A credential
// fingerprint change is the only automatic recovery path for a 401-disabled account.
func (e *CoreEngine) observeCredential(authID, fingerprint string) bool {
	now := e.now()
	e.mu.Lock()
	a := e.accounts[authID]
	if a == nil {
		e.mu.Unlock()
		return false
	}
	changed := a.CredentialFingerprint != fingerprint
	recover401 := a.Disabled401 && fingerprint != "" && fingerprint != a.DisabledFingerprint
	authIndex := a.AuthIndex
	a.CredentialFingerprint = fingerprint
	a.LastCredentialCheckAt = now
	e.persisted[authID] = corePersistedFromAccount(*a)
	_ = e.persistLocked()
	e.mu.Unlock()

	if !recover401 {
		return changed
	}
	if err := e.host.SetAuthDisabled(authIndex, false); err != nil {
		e.recordLog("warn", "account.reenable_401_failed", "credential changed but CPA account re-enable failed", map[string]any{"auth_id": authID, "error": sanitizeCoreError(err)})
		return changed
	}

	e.mu.Lock()
	if a = e.accounts[authID]; a != nil && a.Disabled401 && fingerprint != a.DisabledFingerprint {
		a.Disabled401 = false
		a.Disabled401At = time.Time{}
		a.DisabledFingerprint = ""
		a.LastError = ""
		a.ProbeStatus = "credential_recovered"
		e.persisted[authID] = corePersistedFromAccount(*a)
		_ = e.persistLocked()
		changed = true
	}
	e.mu.Unlock()
	if changed {
		e.recordLog("info", "account.reenabled_after_credential_change", "CPA account re-enabled after credential change", map[string]any{"auth_id": authID})
	}
	return changed
}

func (e *CoreEngine) finishRefreshError(authID, code string, err error) {
	now := e.now()
	e.mu.Lock()
	if a := e.accounts[authID]; a != nil {
		a.LastRefreshAt = now
		a.LastError = code
		e.persisted[authID] = corePersistedFromAccount(*a)
		_ = e.persistLocked()
	}
	e.mu.Unlock()
	e.recordLog("warn", "quota.refresh_failed", "quota refresh failed", map[string]any{"auth_id": authID, "code": code, "error": sanitizeCoreError(err)})
}

func (e *CoreEngine) finishRefreshSuccess(authID string, quota ParsedQuota) {
	now := e.now()
	e.mu.Lock()
	a := e.accounts[authID]
	if a == nil {
		e.mu.Unlock()
		return
	}
	previous := cloneCoreQuota(a.Quota)
	a.Quota = cloneCoreQuota(quota)
	a.LastRefreshAt = now
	a.LastSuccessAt = now
	a.LastError = ""
	if a.BanReason == "usage_429" && coreQuotaConfirmsAvailable(quota) {
		a.BanUntil = time.Time{}
		a.BanReason = ""
	} else {
		e.clearExpiredBanLocked(a, now)
	}
	e.updateProbeScheduleLocked(a, previous, quota, now)
	e.persisted[authID] = corePersistedFromAccount(*a)
	_ = e.persistLocked()
	e.mu.Unlock()
}

func (e *CoreEngine) updateProbeScheduleLocked(a *CoreAccount, previous, current ParsedQuota, now time.Time) {
	if a == nil {
		return
	}
	cfg := e.cfg
	if !cfg.EnableResetProbe {
		a.ProbeBaselineResetAt = time.Time{}
		a.ProbeDueAt = time.Time{}
		if !a.Disabled401 {
			a.ProbeStatus = ""
		}
		return
	}
	if current.FiveHour == nil || current.FiveHour.ResetAt.IsZero() {
		return
	}
	currentReset := current.FiveHour.ResetAt
	if previous.FiveHour != nil && !previous.FiveHour.ResetAt.IsZero() {
		oldReset := previous.FiveHour.ResetAt
		if !oldReset.After(now) && currentReset.After(oldReset.Add(coreResetAdvanceSlop)) {
			// The window advanced naturally. Keep the confirmation status, but
			// immediately schedule maintenance for the *next* known reset rather
			// than clearing the timer and waiting for another post-reset refresh.
			a.ProbeBaselineResetAt = currentReset
			a.ProbeDueAt = currentReset.Add(cfg.ResetProbeAfterResetDelay)
			a.ProbeStatus = "natural_reset_confirmed"
			return
		}
	}
	due := currentReset.Add(cfg.ResetProbeAfterResetDelay)
	// Preserve an existing due time for the same reset baseline. It may be a
	// retry schedule; recomputing reset+delay here could collapse a configured
	// retry back to an already-expired timestamp.
	if coreSameReset(a.ProbeBaselineResetAt, currentReset) && !a.ProbeDueAt.IsZero() {
		return
	}
	a.ProbeBaselineResetAt = currentReset
	a.ProbeDueAt = due
	if currentReset.After(now) {
		a.ProbeStatus = "scheduled_after_reset"
	} else {
		a.ProbeStatus = "waiting_after_reset"
	}
}

func (e *CoreEngine) ProbeAccount(authID string) error {
	account, ok := e.accountByID(authID)
	if !ok || !account.RefreshEnabled() || account.Disabled401 {
		return nil
	}
	cfg := e.Config()
	now := e.now()
	if !cfg.EnableResetProbe || account.ProbeDueAt.IsZero() || account.ProbeDueAt.After(now) {
		return nil
	}
	baselineReset := account.ProbeBaselineResetAt
	if baselineReset.IsZero() {
		e.retryProbe(authID, "missing_reset_baseline")
		return errors.New("reset probe missing baseline reset")
	}
	e.recordLog("info", "quota.reset_probe_precheck", "running read-only quota check before reset probe", map[string]any{"auth_id": authID, "baseline_reset_at": baselineReset})

	// Safety invariant: always perform a read-only quota precheck first. A
	// failed/ambiguous precheck must never fall through to the POST probe.
	if err := e.refreshAccount(authID, false); err != nil {
		e.retryProbe(authID, "precheck_failed")
		return err
	}
	account, ok = e.accountByID(authID)
	if !ok {
		return nil
	}
	if account.Quota.FiveHour == nil || account.Quota.FiveHour.ResetAt.IsZero() {
		e.retryProbe(authID, "precheck_missing_five_hour")
		return errors.New("reset probe precheck returned no five-hour window")
	}
	if account.Quota.FiveHour.ResetAt.After(baselineReset.Add(coreResetAdvanceSlop)) {
		// Natural reset already advanced the window; finishRefreshSuccess has
		// scheduled the next reset, so no POST is needed.
		return nil
	}
	if account.Quota.FiveHour.ResetAt.Before(baselineReset.Add(-coreResetAdvanceSlop)) {
		e.retryProbe(authID, "precheck_reset_ambiguous")
		return errors.New("reset probe precheck returned an unexpected earlier reset")
	}
	if account.Quota.LongWindow != nil && account.Quota.LongWindow.Exhausted {
		e.retryProbe(authID, "precheck_long_window_exhausted")
		return nil
	}
	if account.Disabled401 || !account.RefreshEnabled() {
		return nil
	}

	credentials, fingerprint, err := e.readCredentials(account)
	if err != nil {
		e.retryProbe(authID, "credential_read_failed")
		return err
	}
	e.observeCredential(authID, fingerprint)
	account, _ = e.accountByID(authID)
	if account.Disabled401 {
		return nil
	}

	// Security/compatibility invariant: probe request content is not rewritten by
	// the scheduler. It reuses the existing, reviewed payload byte-for-byte.
	req := coreProbeRequest(credentials)
	e.recordLog("info", "quota.reset_probe_sending", "sending reset probe after lazy reset confirmation", map[string]any{"auth_id": authID, "baseline_reset_at": baselineReset})
	e.mu.Lock()
	if a := e.accounts[authID]; a != nil {
		a.LastProbeAt = now
		a.ProbeStatus = "probe_sending"
		e.persisted[authID] = corePersistedFromAccount(*a)
		_ = e.persistLocked()
	}
	e.mu.Unlock()
	resp, err := e.host.Do(req)
	if err != nil {
		e.retryProbe(authID, "transport_failed")
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		e.retryProbe(authID, "http_401")
		return errors.New("reset probe returned 401")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		e.retryProbe(authID, "rate_limited")
		return errors.New("reset probe returned 429")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e.retryProbe(authID, "http_"+strconv.Itoa(resp.StatusCode))
		return errors.New("reset probe returned HTTP " + strconv.Itoa(resp.StatusCode))
	}

	e.mu.Lock()
	if a := e.accounts[authID]; a != nil {
		a.ProbeStatus = "probe_sent"
		e.persisted[authID] = corePersistedFromAccount(*a)
		_ = e.persistLocked()
	}
	e.mu.Unlock()
	time.Sleep(coreProbeVerifyDelay)
	if err := e.refreshAccount(authID, false); err != nil {
		e.retryProbe(authID, "verify_refresh_failed")
		return err
	}
	updated, ok := e.accountByID(authID)
	if !ok {
		return nil
	}
	if updated.Quota.FiveHour != nil && updated.Quota.FiveHour.ResetAt.After(baselineReset.Add(coreResetAdvanceSlop)) {
		e.recordLog("info", "quota.reset_probe_verified", "reset probe activated a new five-hour window", map[string]any{"auth_id": authID})
		return nil
	}
	e.retryProbe(authID, "window_not_advanced")
	return nil
}

func (e *CoreEngine) retryProbe(authID, reason string) {
	now := e.now()
	e.mu.Lock()
	if a := e.accounts[authID]; a != nil {
		a.ProbeDueAt = now.Add(e.cfg.ResetProbeRetryDelay)
		a.ProbeStatus = reason
		e.persisted[authID] = corePersistedFromAccount(*a)
		_ = e.persistLocked()
	}
	e.mu.Unlock()
}

func (e *CoreEngine) disable401(authID, fingerprint, reason string) {
	if !e.Config().DisableOn401 {
		return
	}
	now := e.now()
	var authIndex string
	e.mu.Lock()
	if a := e.accounts[authID]; a != nil {
		a.Disabled401 = true
		a.Disabled401At = now
		a.DisabledFingerprint = fingerprint
		a.LastError = reason
		a.ProbeDueAt = time.Time{}
		a.ProbeStatus = "disabled_401"
		authIndex = a.AuthIndex
		e.persisted[authID] = corePersistedFromAccount(*a)
		_ = e.persistLocked()
	}
	e.mu.Unlock()
	if authIndex == "" {
		return
	}
	if err := e.host.SetAuthDisabled(authIndex, true); err != nil {
		e.recordLog("error", "account.disable_401_failed", "business 401 detected but CPA account disable failed", map[string]any{"auth_id": authID, "error": sanitizeCoreError(err)})
		return
	}
	e.recordLog("warn", "account.disabled_401", "CPA account disabled after business 401", map[string]any{"auth_id": authID})
}

func (e *CoreEngine) autoban429(authID string, until time.Time, reason string) {
	if !e.Config().AutoBan429 {
		return
	}
	now := e.now()
	if until.IsZero() || !until.After(now) {
		until = now.Add(coreDefault429Ban)
	}
	e.mu.Lock()
	if a := e.accounts[authID]; a != nil {
		if a.BanUntil.After(until) && a.Banned(now) {
			until = a.BanUntil
		}
		a.BanUntil = until
		a.BanReason = reason
		a.LastError = reason
		e.persisted[authID] = corePersistedFromAccount(*a)
		_ = e.persistLocked()
	}
	e.mu.Unlock()
	e.recordLog("warn", "account.autoban_429", "account temporarily banned inside scheduler after 429", map[string]any{"auth_id": authID, "ban_until": until})
}

func (e *CoreEngine) HandleUsage(record pluginapi.UsageRecord) {
	if !strings.EqualFold(record.Provider, "codex") || strings.TrimSpace(record.AuthID) == "" {
		return
	}
	if !record.Failed {
		e.mu.Lock()
		if a := e.accounts[record.AuthID]; a != nil {
			if a.BanReason == "usage_429" {
				a.BanUntil = time.Time{}
				a.BanReason = ""
			} else {
				e.clearExpiredBanLocked(a, e.now())
			}
			e.persisted[record.AuthID] = corePersistedFromAccount(*a)
			_ = e.persistLocked()
		}
		e.mu.Unlock()
		return
	}
	switch record.Failure.StatusCode {
	case http.StatusUnauthorized:
		if account, ok := e.accountByID(record.AuthID); ok {
			fingerprint := account.CredentialFingerprint
			if fingerprint == "" {
				if _, liveFingerprint, err := e.readCredentials(account); err == nil {
					fingerprint = liveFingerprint
					e.observeCredential(record.AuthID, liveFingerprint)
				}
			}
			e.disable401(record.AuthID, fingerprint, "usage_401")
		}
	case http.StatusTooManyRequests:
		e.autoban429(record.AuthID, coreRetryAfter(record.ResponseHeaders, []byte(record.Failure.Body), e.now()), "usage_429")
	}
}

func coreRetryAfter(headers http.Header, body []byte, now time.Time) time.Time {
	if headers != nil {
		if raw := strings.TrimSpace(headers.Get("Retry-After")); raw != "" {
			if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
				return now.Add(time.Duration(seconds) * time.Second)
			}
			if at, err := http.ParseTime(raw); err == nil && at.After(now) {
				return at
			}
		}
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) == nil {
		for _, holder := range []map[string]any{doc, nestedCoreMap(doc, "error")} {
			if holder == nil {
				continue
			}
			if resetAt, ok := getTime(holder, "reset_at", "resetAt"); ok && resetAt.After(now) {
				return resetAt
			}
			if seconds, ok := getFloat64(holder, "resets_in_seconds", "reset_after_seconds", "retry_after"); ok && seconds > 0 {
				return now.Add(time.Duration(seconds * float64(time.Second)))
			}
		}
	}
	return now.Add(coreDefault429Ban)
}

func nestedCoreMap(parent map[string]any, key string) map[string]any {
	if parent == nil {
		return nil
	}
	child, _ := parent[key].(map[string]any)
	return child
}

func coreQuotaConfirmsAvailable(quota ParsedQuota) bool {
	if quota.FiveHour == nil || quota.FiveHour.Exhausted {
		return false
	}
	if quota.LongWindow != nil && quota.LongWindow.Exhausted {
		return false
	}
	return true
}

func coreSameReset(left, right time.Time) bool {
	if left.IsZero() || right.IsZero() {
		return false
	}
	delta := left.Sub(right)
	if delta < 0 {
		delta = -delta
	}
	return delta <= coreResetAdvanceSlop
}

func sanitizeCoreError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	lower := strings.ToLower(text)
	for _, forbidden := range []string{"bearer ", "access_token", "refresh_token", "id_token", "authorization", "cookie"} {
		if strings.Contains(lower, forbidden) {
			return "redacted"
		}
	}
	if len(text) > 240 {
		return text[:240]
	}
	return text
}
