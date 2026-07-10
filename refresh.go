package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const quotaUserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
const resetCreditsEndpoint = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
const codexTokenEndpoint = "https://auth.openai.com/oauth/token"
const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

const maxErrorBodySummaryLen = 220

var (
	rawCookiePattern       = regexp.MustCompile(`(?i)\b(cookie\s*[:=]\s*)[^;\s,}"']+(?:\s*;\s*[^;\s,}"']+)*`)
	rawSecretPattern       = regexp.MustCompile(`(?i)\b((?:access[_-]?token|id[_-]?token|refresh[_-]?token|authorization|cookie|api[_-]?key|session[_-]?token)\s*[:=]\s*)(?:bearer\s+)?[^\s,}"']+`)
	secretFieldPattern     = regexp.MustCompile(`(?i)((?:"?(?:access[_-]?token|id[_-]?token|refresh[_-]?token|authorization|cookie|api[_-]?key|session[_-]?token)"?)\s*[:=]\s*)("[^"]*"|[^\s,}\]]+)`)
	bearerPattern          = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)
	errCPAAdmissionChanged = errors.New("CPA admission changed during refresh")
)

var callHostCallback = func(method string, payload any) (json.RawMessage, error) {
	return nil, fmt.Errorf("host callback %s unavailable", method)
}

type HostClient interface {
	ListAuths() ([]pluginapi.HostAuthFileEntry, error)
	GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error)
	SaveAuth(name string, raw json.RawMessage) error
	Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	Log(level, message string, fields map[string]any)
}

type ABIHostClient struct{}

type QuotaRefresher struct {
	host  HostClient
	state *PluginState
	now   func() time.Time

	mu         sync.Mutex
	running    bool
	refreshing bool
	stopping   bool
	stop       chan struct{}
	done       chan struct{}
	wake       chan struct{}
	pendingDue func() error
	wg         sync.WaitGroup
}

func NewQuotaRefresher(host HostClient, state *PluginState, now func() time.Time) *QuotaRefresher {
	if now == nil {
		now = time.Now
	}
	return &QuotaRefresher{
		host:  host,
		state: state,
		now:   now,
	}
}

func (r *QuotaRefresher) RefreshOnce() error {
	admission, version := r.state.CPAAdmissionVersioned()
	if !admission.Observed {
		return nil
	}
	auths, err := r.host.ListAuths()
	if err != nil {
		return fmt.Errorf("list auths: %w", err)
	}
	if !r.state.CPAAdmissionVersionCurrent(version) {
		return nil
	}
	r.recordAuthScan(auths, admission, version)
	eligible := filterAdmittedAuths(auths, admission)
	if len(eligible) == 0 {
		return nil
	}

	r.refreshAuths(eligible, version)
	return nil
}

func (r *QuotaRefresher) RefreshDueOnce() error {
	admission, version := r.state.CPAAdmissionVersioned()
	if !admission.Observed {
		return nil
	}
	now := r.now()
	if !r.state.RefreshActive(now) {
		return nil
	}
	dueAccounts := r.state.DueAccounts(now)
	dueAuthIDs := make(map[string]struct{}, len(dueAccounts))
	for _, account := range dueAccounts {
		if account.AuthID != "" {
			dueAuthIDs[account.AuthID] = struct{}{}
		}
	}
	knownAuthIDs := knownAccountAuthIDs(r.state.Snapshot(now))
	if len(dueAuthIDs) == 0 && len(knownAuthIDs) > 0 {
		return nil
	}

	auths, err := r.host.ListAuths()
	if err != nil {
		return fmt.Errorf("list auths: %w", err)
	}
	if !r.state.CPAAdmissionVersionCurrent(version) {
		return nil
	}
	r.recordAuthScan(auths, admission, version)
	eligible := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	for _, auth := range filterAdmittedAuths(auths, admission) {
		if len(dueAuthIDs) == 0 {
			eligible = append(eligible, auth)
			continue
		}
		if _, due := dueAuthIDs[auth.ID]; due {
			eligible = append(eligible, auth)
		}
	}
	if len(eligible) == 0 {
		return nil
	}

	r.refreshAuths(eligible, version)
	return nil
}

func (r *QuotaRefresher) RefreshDueCandidatesOnce(req pluginapi.SchedulerPickRequest) error {
	_, version := r.observeCandidateAdmission(req)
	return r.refreshDueCandidatesOnce(version)
}

func (r *QuotaRefresher) observeCandidateAdmission(req pluginapi.SchedulerPickRequest) (CPAAdmissionState, uint64) {
	if admission, ok := HighestPriorityCodexAdmission(req); ok {
		version := r.state.ReplaceCPAAdmission(admission)
		return admission, version
	}
	return r.state.CPAAdmissionVersioned()
}

func (r *QuotaRefresher) refreshDueCandidatesOnce(version uint64) error {
	admission, currentVersion := r.state.CPAAdmissionVersioned()
	if !admission.Observed || currentVersion != version {
		return nil
	}
	now := r.now()
	if !r.state.RefreshActive(now) {
		return nil
	}
	candidateIDs := admission.AuthIDs
	if len(candidateIDs) == 0 {
		return r.RefreshDueOnce()
	}
	dueAccounts := r.state.DueAccounts(now)
	dueAuthIDs := make(map[string]struct{}, len(dueAccounts))
	for _, account := range dueAccounts {
		if account.AuthID != "" {
			dueAuthIDs[account.AuthID] = struct{}{}
		}
	}
	knownAuthIDs := knownAccountAuthIDs(r.state.Snapshot(now))
	if len(dueAuthIDs) == 0 && !hasUnknownCandidate(candidateIDs, knownAuthIDs) {
		return nil
	}
	auths, err := r.host.ListAuths()
	if err != nil {
		return fmt.Errorf("list auths: %w", err)
	}
	if !r.state.CPAAdmissionVersionCurrent(version) {
		return nil
	}
	r.recordAuthScan(auths, admission, version)
	eligible := make([]pluginapi.HostAuthFileEntry, 0, len(candidateIDs))
	for _, auth := range filterAdmittedAuths(auths, admission) {
		if _, candidate := candidateIDs[auth.ID]; !candidate {
			continue
		}
		if len(dueAuthIDs) == 0 {
			if _, known := knownAuthIDs[auth.ID]; !known {
				eligible = append(eligible, auth)
			}
			continue
		}
		if _, due := dueAuthIDs[auth.ID]; due {
			eligible = append(eligible, auth)
			continue
		}
		if _, known := knownAuthIDs[auth.ID]; !known {
			eligible = append(eligible, auth)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	r.refreshAuths(eligible, version)
	return nil
}

func (r *QuotaRefresher) recordAuthScan(auths []pluginapi.HostAuthFileEntry, admission CPAAdmissionState, version uint64) {
	if r.state == nil {
		return
	}
	count := 0
	for _, auth := range auths {
		if _, admitted := admission.AuthIDs[auth.ID]; isRefreshEligible(auth) && admitted {
			count++
		}
	}
	r.state.RecordAuthScanIfAdmissionCurrent(version, count, r.now())
}

func knownAccountAuthIDs(snapshot StateSnapshot) map[string]struct{} {
	ids := make(map[string]struct{}, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if account.AuthID != "" {
			ids[account.AuthID] = struct{}{}
		}
	}
	return ids
}

func hasUnknownCandidate(candidateIDs, knownAuthIDs map[string]struct{}) bool {
	for candidateID := range candidateIDs {
		if _, known := knownAuthIDs[candidateID]; !known {
			return true
		}
	}
	return false
}

func (r *QuotaRefresher) refreshAuths(eligible []pluginapi.HostAuthFileEntry, version uint64) {
	concurrency := r.state.Config().MaxRefreshConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(eligible) {
		concurrency = len(eligible)
	}

	jobs := make(chan pluginapi.HostAuthFileEntry)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for auth := range jobs {
				r.refreshAuthVersioned(auth, version)
			}
		}()
	}
	for _, auth := range eligible {
		jobs <- auth
	}
	close(jobs)
	wg.Wait()
}

func (r *QuotaRefresher) RefreshOneAuthID(authID string) error {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return fmt.Errorf("auth_id is required")
	}
	_, version, admitted := r.state.AdmittedCPAPriorityVersioned(authID)
	if !admitted {
		return fmt.Errorf("auth %s is outside the active CPA priority tier", authID)
	}
	auths, err := r.host.ListAuths()
	if err != nil {
		return fmt.Errorf("list auths: %w", err)
	}
	for _, auth := range auths {
		if auth.ID != authID {
			continue
		}
		if !isRefreshEligible(auth) {
			return fmt.Errorf("auth %s is not eligible for quota refresh", authID)
		}
		r.refreshAuthVersioned(auth, version)
		return nil
	}
	return fmt.Errorf("auth %s not found", authID)
}

func (r *QuotaRefresher) RefreshOneSoon(authID string) {
	_, version, _ := r.state.AdmittedCPAPriorityVersioned(strings.TrimSpace(authID))
	r.mu.Lock()
	if r.refreshing || r.stopping {
		r.mu.Unlock()
		return
	}
	r.refreshing = true
	r.wg.Add(1)
	previousDue := r.state.NextRefreshDueAt(r.now())
	r.mu.Unlock()

	go func() {
		defer r.finishRefresh(previousDue)
		if err := r.RefreshOneAuthID(authID); err != nil {
			r.host.Log("error", "quota refresh failed", map[string]any{"auth_id": authID, "error": redactSecrets(err.Error())})
			if r.state != nil {
				r.recordAdmissionLog(authID, version, "warn", "quota.refresh_one_failed", "账号额度刷新失败", map[string]any{"auth_id": authID, "error": redactSecrets(err.Error())})
			}
		}
	}()
}

func (r *QuotaRefresher) Start() {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	wake := make(chan struct{}, 1)
	r.stop = stop
	r.done = done
	r.wake = wake
	r.running = true
	r.stopping = false
	r.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
		}()

		for {
			interval := r.nextRefreshLoopDelay()
			if interval <= 0 {
				r.RefreshDueSoon()
				interval = minDuration(r.state.Config().QuotaRefreshInterval, time.Second)
			}
			timer := time.NewTimer(interval)
			select {
			case <-timer.C:
				r.RefreshDueSoon()
			case <-wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-stop:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			}
		}
	}()
}

func (r *QuotaRefresher) nextRefreshLoopDelay() time.Duration {
	now := r.now()
	interval := r.state.Config().QuotaRefreshInterval
	if dueAt := r.state.NextRefreshDueAt(now); !dueAt.IsZero() {
		delay := dueAt.Sub(now)
		if delay < interval {
			return delay
		}
	}
	return interval
}

func (r *QuotaRefresher) wakeRefreshLoop() {
	r.mu.Lock()
	wake := r.wake
	running := r.running
	stopping := r.stopping
	r.mu.Unlock()
	if !running || stopping || wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (r *QuotaRefresher) wakeRefreshLoopIfDueAdvanced(previous time.Time) {
	now := r.now()
	next := r.state.NextRefreshDueAt(now)
	if next.IsZero() || !next.After(now) {
		return
	}
	if previous.IsZero() || !previous.After(now) || next.Before(previous) {
		r.wakeRefreshLoop()
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (r *QuotaRefresher) Stop() {
	r.mu.Lock()
	r.stopping = true
	running := r.running
	stop := r.stop
	done := r.done
	if running {
		r.running = false
	}
	r.mu.Unlock()

	if running {
		close(stop)
		<-done
	}
	r.wg.Wait()
}

func (r *QuotaRefresher) RefreshSoon() {
	r.mu.Lock()
	if r.refreshing || r.stopping {
		r.mu.Unlock()
		return
	}
	r.refreshing = true
	r.wg.Add(1)
	previousDue := r.state.NextRefreshDueAt(r.now())
	r.mu.Unlock()

	go func() {
		defer r.finishRefresh(previousDue)
		if err := r.RefreshOnce(); err != nil {
			r.host.Log("error", "quota refresh failed", map[string]any{"error": redactSecrets(err.Error())})
		}
	}()
}

func (r *QuotaRefresher) RefreshDueSoon() {
	r.refreshDueSoon(func() error {
		return r.RefreshDueOnce()
	})
}

func (r *QuotaRefresher) RefreshDueCandidatesSoon(req pluginapi.SchedulerPickRequest) {
	_, version := r.observeCandidateAdmission(req)
	r.refreshDueSoon(func() error {
		return r.refreshDueCandidatesOnce(version)
	})
}

func (r *QuotaRefresher) refreshDueSoon(refresh func() error) {
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return
	}
	if r.refreshing {
		r.pendingDue = refresh
		r.mu.Unlock()
		return
	}
	r.refreshing = true
	r.wg.Add(1)
	previousDue := r.state.NextRefreshDueAt(r.now())
	r.mu.Unlock()

	go func() {
		defer r.finishRefresh(previousDue)
		if err := refresh(); err != nil {
			r.host.Log("error", "quota due refresh failed", map[string]any{"error": redactSecrets(err.Error())})
		}
	}()
}

func (r *QuotaRefresher) finishRefresh(previousDue time.Time) {
	r.mu.Lock()
	pending := r.pendingDue
	if r.stopping || pending == nil {
		r.pendingDue = nil
		r.refreshing = false
		r.mu.Unlock()
		r.wg.Done()
		r.wakeRefreshLoopIfDueAdvanced(previousDue)
		return
	}
	r.pendingDue = nil
	r.mu.Unlock()
	r.wakeRefreshLoopIfDueAdvanced(previousDue)
	go func() {
		defer r.finishRefresh(previousDue)
		if err := pending(); err != nil {
			r.host.Log("error", "quota due refresh failed", map[string]any{"error": redactSecrets(err.Error())})
		}
	}()
}

func (r *QuotaRefresher) refreshAuth(auth pluginapi.HostAuthFileEntry) {
	_, version, admitted := r.state.AdmittedCPAPriorityVersioned(auth.ID)
	if admitted {
		r.refreshAuthVersioned(auth, version)
	}
}

func (r *QuotaRefresher) refreshAuthVersioned(auth pluginapi.HostAuthFileEntry, version uint64) {
	priority, currentVersion, admitted := r.state.AdmittedCPAPriorityVersioned(auth.ID)
	if !admitted || currentVersion != version {
		return
	}
	account := accountStateFromAuth(auth, r.now())
	account.Priority = priority

	authResp, err := r.host.GetAuth(auth.AuthIndex)
	if err != nil {
		r.upsertRefreshFailure(account, version, redactSecrets(fmt.Sprintf("get auth: %v", err)), RefreshFailureTransient)
		return
	}

	credentials, err := ExtractCodexCredentials(authResp.JSON)
	if err != nil {
		r.upsertRefreshFailure(account, version, redactWithCredentials(fmt.Sprintf("extract credentials: %v", err), credentials), RefreshFailureLocal)
		return
	}
	if accessTokenExpired(credentials, r.now()) {
		credentials, err = r.refreshAndSaveCredentials(authResp, credentials, account.AuthID, version)
		if err != nil {
			if errors.Is(err, errCPAAdmissionChanged) {
				return
			}
			r.upsertRefreshFailure(account, version, redactWithCredentials(fmt.Sprintf("refresh access token: %v", err), credentials), refreshTokenFailureKind(err))
			return
		}
	}
	account = r.mergeExistingAccount(account)
	account.ChatGPTAccountID = credentials.ChatGPTAccountID
	previous := account

	cfg := r.state.Config()
	quota, err := r.fetchQuota(credentials)
	if err != nil {
		kind := RefreshFailureTransient
		var statusErr quotaStatusError
		if errors.As(err, &statusErr) {
			kind = refreshFailureKind(statusErr.status)
		}
		r.upsertRefreshFailure(account, version, redactWithCredentials(err.Error(), credentials), kind)
		return
	}
	if resetCredits, err := r.refreshResetCredits(credentials); err != nil {
		r.recordAdmissionLog(account.AuthID, version, "warn", "quota.reset_credits_failed", "主动重置次数刷新失败", map[string]any{"auth_id": account.AuthID, "error": redactWithCredentials(err.Error(), credentials)})
	} else {
		mergeResetCredits(&quota, resetCredits)
	}
	if cfg.EnableResetProbe {
		account.ResetProbes = scheduleResetProbesFromPrevious(previous, previous.ResetProbes, r.now())
		lazyKinds := make([]WindowKind, 0, len(account.ResetProbes))
		for kind, probe := range account.ResetProbes {
			if !resetProbeDue(probe, r.now()) {
				continue
			}
			window, ok := matchingProbeWindow(quota, probe)
			if !ok || window.ResetAt.IsZero() {
				account.ResetProbes[kind] = markResetProbeRetry(probe, r.now(), errors.New("matching quota window missing"), credentials, cfg)
				continue
			}
			if looksLikeLazyReset(r.now(), window, probe.WindowSeconds) {
				lazyKinds = append(lazyKinds, kind)
				continue
			}
			account.ResetProbes[kind] = markResetProbeConfirmedActive(probe)
		}
		if len(lazyKinds) > 0 {
			if err := r.runResetProbe(credentials); err != nil {
				for _, kind := range lazyKinds {
					account.ResetProbes[kind] = markResetProbeRetry(account.ResetProbes[kind], r.now(), err, credentials, cfg)
				}
				r.recordAdmissionLog(account.AuthID, version, "warn", "quota.reset_probe_failed", "Codex reset probe failed", map[string]any{"auth_id": account.AuthID, "error": redactWithCredentials(err.Error(), credentials)})
			} else {
				for _, kind := range lazyKinds {
					account.ResetProbes[kind] = markResetProbeVerified(account.ResetProbes[kind], r.now())
				}
				r.recordAdmissionLog(account.AuthID, version, "info", "quota.reset_probe_verified", "Codex reset probe verified", map[string]any{"auth_id": account.AuthID})
				if refreshedQuota, err := r.fetchQuota(credentials); err != nil {
					r.recordAdmissionLog(account.AuthID, version, "warn", "quota.reset_probe_post_refresh_failed", "Post-probe quota refresh failed", map[string]any{"auth_id": account.AuthID, "error": redactWithCredentials(err.Error(), credentials)})
				} else {
					quota = refreshedQuota
					if resetCredits, err := r.refreshResetCredits(credentials); err != nil {
						r.recordAdmissionLog(account.AuthID, version, "warn", "quota.reset_credits_failed", "主动重置次数刷新失败", map[string]any{"auth_id": account.AuthID, "error": redactWithCredentials(err.Error(), credentials)})
					} else {
						mergeResetCredits(&quota, resetCredits)
					}
				}
			}
		}
	}
	account.Quota = quota
	account.Family = quota.Family
	account.LastError = ""
	account.LastSuccessAt = r.now()
	if r.state.ApplyQuotaRefreshSuccessIfAdmissionCurrent(account, version, r.now()) {
		r.recordAdmissionLog(account.AuthID, version, "info", "quota.refresh_success", "账号额度刷新成功", map[string]any{"auth_id": account.AuthID})
	}
}

func (r *QuotaRefresher) recordAdmissionLog(authID string, version uint64, level, event, message string, fields map[string]any) {
	r.state.RecordLogIfAdmissionCurrent(authID, version, level, event, message, fields, r.now())
}

func (r *QuotaRefresher) fetchQuota(credentials CodexCredentials) (ParsedQuota, error) {
	cfg := r.state.Config()
	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    cfg.QuotaEndpoint,
		Headers: http.Header{
			"Authorization":      []string{"Bearer " + credentials.AccessToken},
			"Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID},
			"Content-Type":       []string{"application/json"},
			"User-Agent":         []string{quotaUserAgent},
		},
	}
	resp, err := r.host.Do(req)
	if err != nil {
		return ParsedQuota{}, fmt.Errorf("quota request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ParsedQuota{}, quotaStatusError{status: resp.StatusCode, body: resp.Body}
	}
	quota, err := ParseCodexUsagePayload(resp.Body, r.now())
	if err != nil {
		return ParsedQuota{}, fmt.Errorf("parse quota: %w", err)
	}
	return quota, nil
}

type quotaStatusError struct {
	status int
	body   []byte
}

func (e quotaStatusError) Error() string {
	return fmt.Sprintf("quota request returned status %d: response body: %s", e.status, sanitizedBodySummary(e.body))
}

func (r *QuotaRefresher) runResetProbe(credentials CodexCredentials) error {
	req := pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    codexResetProbeEndpoint,
		Headers: http.Header{
			"Authorization":      []string{"Bearer " + credentials.AccessToken},
			"Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID},
			"Accept":             []string{"application/json"},
			"Content-Type":       []string{"application/json"},
			"User-Agent":         []string{quotaUserAgent},
		},
		Body: resetProbePayloadBytes(),
	}
	resp, err := r.host.Do(req)
	if err != nil {
		return fmt.Errorf("reset probe request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("reset probe returned status %d: response body: %s", resp.StatusCode, sanitizedBodySummary(resp.Body))
	}
	if !resetProbeUsageEvidence(resp.Body) {
		return errors.New("reset probe response did not include usage evidence")
	}
	return nil
}

func (r *QuotaRefresher) refreshAndSaveCredentials(authResp pluginapi.HostAuthGetResponse, current CodexCredentials, authID string, version uint64) (CodexCredentials, error) {
	if strings.TrimSpace(current.RefreshToken) == "" {
		return CodexCredentials{}, errors.New("access_token expired and refresh_token is missing")
	}
	refreshed, err := r.refreshCodexAccessToken(current.RefreshToken)
	if err != nil {
		return CodexCredentials{}, err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = current.RefreshToken
	}
	if refreshed.ChatGPTAccountID == "" {
		refreshed.ChatGPTAccountID = current.ChatGPTAccountID
	}
	if refreshed.IDToken == "" {
		refreshed.IDToken = current.IDToken
	}
	if refreshed.AccessToken == "" {
		return CodexCredentials{}, errors.New("refresh response did not include access_token")
	}

	updated, err := updateCodexAuthJSON(authResp.JSON, refreshed, r.now())
	if err != nil {
		return CodexCredentials{}, err
	}
	name := strings.TrimSpace(authResp.Name)
	if name == "" {
		return CodexCredentials{}, errors.New("auth file name is required to save refreshed token")
	}
	_, currentVersion, admitted := r.state.AdmittedCPAPriorityVersioned(authID)
	if !admitted || currentVersion != version {
		return CodexCredentials{}, errCPAAdmissionChanged
	}
	if err := r.host.SaveAuth(name, updated); err != nil {
		return CodexCredentials{}, err
	}
	r.recordAdmissionLog(authID, version, "info", "quota.token_refreshed", "Codex access token refreshed and saved", map[string]any{"auth_file": name})
	return refreshed, nil
}

func (r *QuotaRefresher) refreshCodexAccessToken(refreshToken string) (CodexCredentials, error) {
	form := url.Values{
		"client_id":     {codexClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}
	req := pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    codexTokenEndpoint,
		Headers: http.Header{
			"Accept":       []string{"application/json"},
			"Content-Type": []string{"application/x-www-form-urlencoded"},
		},
		Body: []byte(form.Encode()),
	}
	resp, err := r.host.Do(req)
	if err != nil {
		return CodexCredentials{}, fmt.Errorf("token refresh request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CodexCredentials{}, tokenRefreshStatusError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("token refresh returned status %d: response body: %s", resp.StatusCode, sanitizedBodySummary(resp.Body)),
		}
	}
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		AccountID    string `json:"account_id"`
	}
	if err := json.Unmarshal(resp.Body, &tokenResp); err != nil {
		return CodexCredentials{}, fmt.Errorf("parse token refresh response: %w", err)
	}
	credentials := CodexCredentials{
		AccessToken:      strings.TrimSpace(tokenResp.AccessToken),
		RefreshToken:     strings.TrimSpace(tokenResp.RefreshToken),
		IDToken:          strings.TrimSpace(tokenResp.IDToken),
		ChatGPTAccountID: strings.TrimSpace(tokenResp.AccountID),
	}
	if credentials.ChatGPTAccountID == "" && credentials.IDToken != "" {
		if accountID, err := extractChatGPTAccountID(credentials.IDToken); err == nil {
			credentials.ChatGPTAccountID = accountID
		}
	}
	if tokenResp.ExpiresIn > 0 {
		credentials.ExpiresAt = r.now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	}
	return credentials, nil
}

type tokenRefreshStatusError struct {
	status int
	msg    string
}

func (e tokenRefreshStatusError) Error() string { return e.msg }

func refreshTokenFailureKind(err error) RefreshFailureKind {
	var statusErr tokenRefreshStatusError
	if errors.As(err, &statusErr) {
		if statusErr.status == http.StatusBadRequest || statusErr.status == http.StatusUnauthorized || strings.Contains(strings.ToLower(statusErr.msg), "invalid_grant") {
			return RefreshFailureAuth
		}
		return refreshFailureKind(statusErr.status)
	}
	if strings.Contains(err.Error(), "refresh_token is missing") ||
		strings.Contains(err.Error(), "auth file name is required") ||
		strings.Contains(err.Error(), "refresh response did not include access_token") {
		return RefreshFailureLocal
	}
	return RefreshFailureTransient
}

func updateCodexAuthJSON(raw json.RawMessage, credentials CodexCredentials, now time.Time) (json.RawMessage, error) {
	var doc map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
	}
	if doc == nil {
		doc = make(map[string]any)
	}
	doc["access_token"] = credentials.AccessToken
	doc["refresh_token"] = credentials.RefreshToken
	doc["id_token"] = credentials.IDToken
	doc["account_id"] = credentials.ChatGPTAccountID
	doc["type"] = "codex"
	if !credentials.ExpiresAt.IsZero() {
		doc["expired"] = credentials.ExpiresAt.Format(time.RFC3339)
	}
	if !now.IsZero() {
		doc["last_refresh"] = now.Format(time.RFC3339)
	}
	return json.Marshal(doc)
}

func (r *QuotaRefresher) refreshResetCredits(credentials CodexCredentials) (ParsedQuota, error) {
	req := pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    resetCreditsEndpoint,
		Headers: http.Header{
			"Authorization":      []string{"Bearer " + credentials.AccessToken},
			"Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID},
			"Content-Type":       []string{"application/json"},
			"User-Agent":         []string{quotaUserAgent},
		},
	}
	resp, err := r.host.Do(req)
	if err != nil {
		return ParsedQuota{}, fmt.Errorf("reset credits request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ParsedQuota{}, fmt.Errorf("reset credits request returned status %d: response body: %s", resp.StatusCode, sanitizedBodySummary(resp.Body))
	}
	quota, err := ParseResetCreditsPayload(resp.Body, r.now())
	if err != nil {
		return ParsedQuota{}, fmt.Errorf("parse reset credits: %w", err)
	}
	return quota, nil
}

func mergeResetCredits(quota *ParsedQuota, resetCredits ParsedQuota) {
	if resetCredits.ResetCreditsAvailableCount != nil {
		count := *resetCredits.ResetCreditsAvailableCount
		quota.ResetCreditsAvailableCount = &count
	}
	if resetCredits.ResetCreditsTotalEarnedCount != nil {
		count := *resetCredits.ResetCreditsTotalEarnedCount
		quota.ResetCreditsTotalEarnedCount = &count
	}
	if len(resetCredits.ResetCredits) > 0 {
		quota.ResetCredits = append([]ResetCredit(nil), resetCredits.ResetCredits...)
	}
}

func refreshFailureKind(status int) RefreshFailureKind {
	if status == http.StatusUnauthorized {
		return RefreshFailureAuth
	}
	return RefreshFailureTransient
}

func (r *QuotaRefresher) upsertRefreshFailure(account AccountState, version uint64, message string, kind RefreshFailureKind) {
	merged := r.mergeExistingAccount(account)
	merged.LastRefreshAt = account.LastRefreshAt
	merged.LastError = message
	if r.state.ApplyQuotaRefreshFailureIfAdmissionCurrent(merged, version, kind, message, r.now()) {
		r.recordAdmissionLog(merged.AuthID, version, "warn", "quota.refresh_failed", "账号额度刷新失败", map[string]any{"auth_id": merged.AuthID, "error": message})
	}
}

func (r *QuotaRefresher) mergeExistingAccount(account AccountState) AccountState {
	key := accountStateKey(account)
	if key == "" {
		return account
	}
	for _, existing := range r.state.Snapshot(r.now()).Accounts {
		if accountStateKey(existing) != key {
			continue
		}
		merged := existing
		merged.AuthID = account.AuthID
		merged.AuthIndex = account.AuthIndex
		merged.DisplayName = account.DisplayName
		merged.Email = account.Email
		merged.Provider = account.Provider
		merged.Priority = account.Priority
		if account.ChatGPTAccountID != "" {
			merged.ChatGPTAccountID = account.ChatGPTAccountID
		}
		return merged
	}
	return account
}

func accountStateFromAuth(auth pluginapi.HostAuthFileEntry, now time.Time) AccountState {
	return AccountState{
		AuthID:        auth.ID,
		AuthIndex:     auth.AuthIndex,
		DisplayName:   auth.Label,
		Email:         auth.Email,
		Provider:      auth.Provider,
		Priority:      auth.Priority,
		LastRefreshAt: now,
	}
}

func isRefreshEligible(auth pluginapi.HostAuthFileEntry) bool {
	return strings.EqualFold(auth.Provider, "codex") &&
		!auth.Disabled &&
		!auth.Unavailable &&
		auth.AuthIndex != ""
}

func filterAdmittedAuths(auths []pluginapi.HostAuthFileEntry, admission CPAAdmissionState) []pluginapi.HostAuthFileEntry {
	filtered := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	for _, auth := range auths {
		if _, admitted := admission.AuthIDs[auth.ID]; isRefreshEligible(auth) && admitted {
			filtered = append(filtered, auth)
		}
	}
	return filtered
}

func redactWithCredentials(message string, credentials CodexCredentials) string {
	message = redactSecrets(message)
	for _, secret := range []string{credentials.AccessToken, credentials.IDToken} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}

func redactSecrets(message string) string {
	message = rawCookiePattern.ReplaceAllString(message, `${1}[redacted]`)
	message = rawSecretPattern.ReplaceAllString(message, `${1}[redacted]`)
	message = secretFieldPattern.ReplaceAllString(message, `${1}"[redacted]"`)
	message = bearerPattern.ReplaceAllString(message, "Bearer [redacted]")
	return message
}

func sanitizedBodySummary(body []byte) string {
	if len(body) == 0 {
		return "<empty>"
	}
	summary := redactSecrets(string(body))
	summary = strings.Join(strings.Fields(summary), " ")
	if len(summary) > maxErrorBodySummaryLen {
		summary = summary[:maxErrorBodySummaryLen] + "..."
	}
	return summary
}

func (ABIHostClient) ListAuths() ([]pluginapi.HostAuthFileEntry, error) {
	result, err := callHostCallback(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list result: %w", err)
	}
	return resp.Files, nil
}

func (ABIHostClient) GetAuth(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	result, err := callHostCallback(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return pluginapi.HostAuthGetResponse{}, err
	}
	var resp pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return pluginapi.HostAuthGetResponse{}, fmt.Errorf("decode host.auth.get result: %w", err)
	}
	return resp, nil
}

func (ABIHostClient) SaveAuth(name string, raw json.RawMessage) error {
	_, err := callHostCallback(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: raw})
	return err
}

func (ABIHostClient) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	result, err := callHostCallback(pluginabi.MethodHostHTTPDo, req)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var resp pluginapi.HTTPResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host.http.do result: %w", err)
	}
	return resp, nil
}

func (ABIHostClient) Log(level, message string, fields map[string]any) {
	_, _ = callHostCallback(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	})
}
