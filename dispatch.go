package main

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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
	engine := ensureCurrentCore()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
		}
		if err := engine.Configure(req.ConfigYAML); err != nil {
			return nil, err
		}
		engine.Start()
		engine.Wake()
		return okEnvelope(PluginRegistration())
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
		}
		if len(engine.Accounts()) == 0 {
			_ = engine.SyncRoster()
		}
		return okEnvelope(engine.Pick(req))
	case pluginabi.MethodUsageHandle:
		var record pluginapi.UsageRecord
		if len(request) > 0 {
			if err := json.Unmarshal(request, &record); err != nil {
				return nil, err
			}
		}
		engine.HandleUsage(record)
		return okEnvelope(map[string]any{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(CoreRegisterManagement())
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
		}
		return okEnvelope(CoreHandleManagement(engine, req))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
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
