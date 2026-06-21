package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const quotaUserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"

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
	for _, auth := range auths {
		if !isRefreshEligible(auth) {
			continue
		}
		r.refreshAuth(auth)
	}
	return nil
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
				r.RefreshSoon()
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
	account.Quota = quota
	account.Family = quota.Family
	account.LastError = ""
	account.LastSuccessAt = r.now()
	r.state.UpsertQuota(account)
}

func (r *QuotaRefresher) upsertRefreshFailure(account AccountState, message string) {
	merged := r.mergeExistingAccount(account)
	merged.LastRefreshAt = account.LastRefreshAt
	merged.LastError = message
	r.state.UpsertQuota(merged)
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
