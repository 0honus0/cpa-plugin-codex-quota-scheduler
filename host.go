package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	SetAuthDisabled(authIndex string, disabled bool) error
	Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	Log(level, message string, fields map[string]any)
}

type ABIHostClient struct{}

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

// SetAuthDisabled is intentionally the only credential mutation exposed to the
// scheduler. It preserves all existing JSON fields and only changes the CPA
// disabled flag, which is required for persistent 401 fail-closed behavior.
func (c ABIHostClient) SetAuthDisabled(authIndex string, disabled bool) error {
	resp, err := c.GetAuth(authIndex)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(resp.Name)
	if name == "" {
		return errors.New("host auth has no physical file name")
	}
	var doc map[string]any
	if err := json.Unmarshal(resp.JSON, &doc); err != nil {
		return fmt.Errorf("decode auth JSON for disabled update: %w", err)
	}
	if doc == nil {
		return errors.New("auth JSON must be an object")
	}
	doc["disabled"] = disabled
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode auth JSON for disabled update: %w", err)
	}
	_, err = callHostCallback(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: raw})
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
		"level": level, "message": message, "fields": fields,
	})
}
