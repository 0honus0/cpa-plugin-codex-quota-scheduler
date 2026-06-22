package main

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	currentConfig   atomic.Value
	globalState     = NewPluginState(DefaultConfig())
	refresherMu     sync.Mutex
	globalRefresher *QuotaRefresher
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		startGlobalRefresher()
		return okEnvelope(PluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	case pluginabi.MethodUsageHandle:
		return handleUsageHandle(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(RegisterManagement())
	case pluginabi.MethodManagementHandle:
		return handleManagementHandle(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg, err := DecodeConfig(req.ConfigYAML)
	if err != nil {
		return err
	}
	var annotations AnnotationState
	if cfg.AnnotationStatePath != "" {
		annotations, err = LoadAnnotations(cfg.AnnotationStatePath)
		if err != nil {
			annotations = NormalizeAnnotationState(AnnotationState{})
		}
	}
	currentConfig.Store(cfg)
	globalState.ReplaceConfig(cfg)
	if cfg.AnnotationStatePath != "" {
		globalState.SetAnnotations(annotations)
	}
	return nil
}

func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	decision := PickCodexAccount(req, globalState.Snapshot(now), now)
	if decision.AuthID != "" {
		globalState.RecordSelection(decision.AuthID, decision.Reason)
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:          decision.AuthID,
		DelegateBuiltin: decision.DelegateBuiltin,
		Handled:         decision.Handled,
	})
}

func handleUsageHandle(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
	}
	HandleUsageFeedback(globalState, record, time.Now())
	return okEnvelope(map[string]any{})
}

func handleManagementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	return okEnvelope(HandleManagementRequest(globalState, req, time.Now()))
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func startGlobalRefresher() {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher == nil {
		return
	}
	refresher.Start()
	refresher.RefreshSoon()
}

func refreshGlobalRefresherSoon() {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher != nil {
		refresher.RefreshSoon()
	}
}
