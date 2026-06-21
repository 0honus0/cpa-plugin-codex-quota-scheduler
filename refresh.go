package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const quotaUserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"

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
	stop       chan struct{}
	done       chan struct{}
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
	r.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
		}()

		r.RefreshSoon()
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
	if !r.running {
		r.mu.Unlock()
		return
	}
	stop := r.stop
	done := r.done
	r.mu.Unlock()

	close(stop)
	<-done
}

func (r *QuotaRefresher) RefreshSoon() {
	r.mu.Lock()
	if r.refreshing {
		r.mu.Unlock()
		return
	}
	r.refreshing = true
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.refreshing = false
			r.mu.Unlock()
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
		account.LastError = redactSecrets(fmt.Sprintf("get auth: %v", err))
		r.state.UpsertQuota(account)
		return
	}

	credentials, err := ExtractCodexCredentials(authResp.JSON)
	if err != nil {
		account.LastError = redactWithCredentials(fmt.Sprintf("extract credentials: %v", err), credentials)
		r.state.UpsertQuota(account)
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
		account.LastError = redactWithCredentials(fmt.Sprintf("quota request: %v", err), credentials)
		r.state.UpsertQuota(account)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		account.LastError = redactWithCredentials(fmt.Sprintf("quota request returned status %d: %s", resp.StatusCode, string(resp.Body)), credentials)
		r.state.UpsertQuota(account)
		return
	}

	quota, err := ParseCodexUsagePayload(resp.Body, r.now())
	if err != nil {
		account.LastError = redactWithCredentials(fmt.Sprintf("parse quota: %v", err), credentials)
		r.state.UpsertQuota(account)
		return
	}
	account.Quota = quota
	account.Family = quota.Family
	account.LastError = ""
	account.LastSuccessAt = r.now()
	r.state.UpsertQuota(account)
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
	fields := []string{"access_token", "id_token", "refresh_token"}
	for _, field := range fields {
		for {
			key := `"` + field + `":"`
			start := strings.Index(message, key)
			if start < 0 {
				break
			}
			valueStart := start + len(key)
			valueEnd := strings.Index(message[valueStart:], `"`)
			if valueEnd < 0 {
				break
			}
			message = message[:valueStart] + "[redacted]" + message[valueStart+valueEnd:]
		}
	}
	if idx := strings.Index(message, "Bearer "); idx >= 0 {
		end := idx + len("Bearer ")
		for end < len(message) && message[end] != ' ' && message[end] != '\n' && message[end] != '\t' && message[end] != '\r' {
			end++
		}
		message = message[:idx+len("Bearer ")] + "[redacted]" + message[end:]
	}
	return message
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
