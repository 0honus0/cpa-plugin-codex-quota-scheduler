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
	rawCookiePattern   = regexp.MustCompile(`(?i)\b(cookie\s*[:=]\s*)[^;\s,}"']+(?:\s*;\s*[^;\s,}"']+)*`)
	rawSecretPattern   = regexp.MustCompile(`(?i)\b((?:access[_-]?token|id[_-]?token|refresh[_-]?token|authorization|cookie|api[_-]?key|session[_-]?token)\s*[:=]\s*)(?:bearer\s+)?[^\s,}"']+`)
	secretFieldPattern = regexp.MustCompile(`(?i)((?:"?(?:access[_-]?token|id[_-]?token|refresh[_-]?token|authorization|cookie|api[_-]?key|session[_-]?token)"?)\s*[:=]\s*)("[^"]*"|[^\s,}\]]+)`)
	bearerPattern      = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)
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
	auths, err := r.host.ListAuths()
	if err != nil {
		return fmt.Errorf("list auths: %w", err)
	}
	eligible := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	for _, auth := range auths {
		if isRefreshEligible(auth) {
			eligible = append(eligible, auth)
		}
	}
	if len(eligible) == 0 {
		return nil
	}

	r.refreshAuths(eligible)
	return nil
}

func (r *QuotaRefresher) RefreshDueOnce() error {
	now := r.now()
	if !r.state.RefreshActive(now) {
		return nil
	}
	dueAccounts := r.state.DueAccounts(now)
	if len(dueAccounts) == 0 {
		return nil
	}
	dueAuthIDs := make(map[string]struct{}, len(dueAccounts))
	for _, account := range dueAccounts {
		if account.AuthID != "" {
			dueAuthIDs[account.AuthID] = struct{}{}
		}
	}
	if len(dueAuthIDs) == 0 {
		return nil
	}

	auths, err := r.host.ListAuths()
	if err != nil {
		return fmt.Errorf("list auths: %w", err)
	}
	eligible := make([]pluginapi.HostAuthFileEntry, 0, len(dueAuthIDs))
	for _, auth := range auths {
		if _, due := dueAuthIDs[auth.ID]; due && isRefreshEligible(auth) {
			eligible = append(eligible, auth)
		}
	}
	if len(eligible) == 0 {
		return nil
	}

	r.refreshAuths(eligible)
	return nil
}

func (r *QuotaRefresher) refreshAuths(eligible []pluginapi.HostAuthFileEntry) {
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
				r.refreshAuth(auth)
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
		r.refreshAuth(auth)
		return nil
	}
	return fmt.Errorf("auth %s not found", authID)
}

func (r *QuotaRefresher) RefreshOneSoon(authID string) {
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return
	}
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		if err := r.RefreshOneAuthID(authID); err != nil {
			r.host.Log("error", "quota refresh failed", map[string]any{"auth_id": authID, "error": redactSecrets(err.Error())})
			if r.state != nil {
				r.state.RecordLog("warn", "quota.refresh_one_failed", "账号额度刷新失败", map[string]any{"auth_id": authID, "error": redactSecrets(err.Error())}, r.now())
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
	r.stop = stop
	r.done = done
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
			interval := r.state.Config().QuotaRefreshInterval
			timer := time.NewTimer(interval)
			select {
			case <-timer.C:
				r.RefreshDueSoon()
			case <-stop:
				if !timer.Stop() {
					<-timer.C
				}
				return
			}
		}
	}()
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
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.refreshing = false
			r.mu.Unlock()
			r.wg.Done()
		}()
		if err := r.RefreshOnce(); err != nil {
			r.host.Log("error", "quota refresh failed", map[string]any{"error": redactSecrets(err.Error())})
		}
	}()
}

func (r *QuotaRefresher) RefreshDueSoon() {
	r.mu.Lock()
	if r.refreshing || r.stopping {
		r.mu.Unlock()
		return
	}
	r.refreshing = true
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.refreshing = false
			r.mu.Unlock()
			r.wg.Done()
		}()
		if err := r.RefreshDueOnce(); err != nil {
			r.host.Log("error", "quota due refresh failed", map[string]any{"error": redactSecrets(err.Error())})
		}
	}()
}

func (r *QuotaRefresher) refreshAuth(auth pluginapi.HostAuthFileEntry) {
	account := accountStateFromAuth(auth, r.now())

	authResp, err := r.host.GetAuth(auth.AuthIndex)
	if err != nil {
		r.upsertRefreshFailure(account, redactSecrets(fmt.Sprintf("get auth: %v", err)))
		return
	}

	credentials, err := ExtractCodexCredentials(authResp.JSON)
	if err != nil {
		r.upsertRefreshFailure(account, redactWithCredentials(fmt.Sprintf("extract credentials: %v", err), credentials))
		return
	}
	if accessTokenExpired(credentials, r.now()) {
		credentials, err = r.refreshAndSaveCredentials(authResp, credentials)
		if err != nil {
			r.upsertRefreshFailure(account, redactWithCredentials(fmt.Sprintf("refresh access token: %v", err), credentials))
			return
		}
	}
	account.ChatGPTAccountID = credentials.ChatGPTAccountID

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
		r.upsertRefreshFailure(account, redactWithCredentials(fmt.Sprintf("quota request: %v", err), credentials))
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("quota request returned status %d: response body: %s", resp.StatusCode, sanitizedBodySummary(resp.Body))
		r.upsertRefreshFailure(account, redactWithCredentials(message, credentials))
		return
	}

	quota, err := ParseCodexUsagePayload(resp.Body, r.now())
	if err != nil {
		r.upsertRefreshFailure(account, redactWithCredentials(fmt.Sprintf("parse quota: %v", err), credentials))
		return
	}
	if resetCredits, err := r.refreshResetCredits(credentials); err != nil {
		r.state.RecordLog("warn", "quota.reset_credits_failed", "主动重置次数刷新失败", map[string]any{"auth_id": account.AuthID, "error": redactWithCredentials(err.Error(), credentials)}, r.now())
	} else {
		mergeResetCredits(&quota, resetCredits)
	}
	account.Quota = quota
	account.Family = quota.Family
	account.LastError = ""
	account.LastSuccessAt = r.now()
	r.state.UpsertQuota(account)
	r.state.RecordLog("info", "quota.refresh_success", "账号额度刷新成功", map[string]any{"auth_id": account.AuthID}, r.now())
}

func (r *QuotaRefresher) refreshAndSaveCredentials(authResp pluginapi.HostAuthGetResponse, current CodexCredentials) (CodexCredentials, error) {
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
	if err := r.host.SaveAuth(name, updated); err != nil {
		return CodexCredentials{}, err
	}
	r.state.RecordLog("info", "quota.token_refreshed", "Codex access token refreshed and saved", map[string]any{"auth_file": name}, r.now())
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
		return CodexCredentials{}, fmt.Errorf("token refresh returned status %d: response body: %s", resp.StatusCode, sanitizedBodySummary(resp.Body))
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

func (r *QuotaRefresher) upsertRefreshFailure(account AccountState, message string) {
	merged := r.mergeExistingAccount(account)
	merged.LastRefreshAt = account.LastRefreshAt
	merged.LastError = message
	r.state.UpsertQuota(merged)
	r.state.RecordLog("warn", "quota.refresh_failed", "账号额度刷新失败", map[string]any{"auth_id": merged.AuthID, "error": message}, r.now())
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
