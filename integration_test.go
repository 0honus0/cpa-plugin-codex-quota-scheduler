//go:build cgo

package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHandleMethodRegisterEnvelope(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodPluginRegister, []byte(`{"config_yaml":"aGFuZGxlX2VuYWJsZWQ6IHRydWUK"}`))
	if err != nil {
		t.Fatalf("handle register: %v", err)
	}

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got %+v", env)
	}
}

func TestHandleMethodSchedulerPickFallbackEnvelope(t *testing.T) {
	globalState = NewPluginState(DefaultConfig())
	rawReq, err := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "missing", Provider: "codex", Priority: 1},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	raw, err := handleMethod(pluginabi.MethodSchedulerPick, rawReq)
	if err != nil {
		t.Fatalf("handle scheduler pick: %v", err)
	}

	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got %+v", env)
	}

	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode scheduler response: %v", err)
	}
	if !resp.Handled || resp.DelegateBuiltin != pluginapi.SchedulerBuiltinFillFirst {
		t.Fatalf("expected fill-first fallback, got %+v", resp)
	}
}
