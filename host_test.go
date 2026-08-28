package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestABIHostClientSetAuthDisabledPreservesCredentialFields(t *testing.T) {
	previous := callHostCallback
	t.Cleanup(func() { callHostCallback = previous })

	original := json.RawMessage(`{"type":"codex","email":"user@example.test","access_token":"access-secret","refresh_token":"refresh-secret","account_id":"acct-1","nested":{"keep":"yes"}}`)
	var saved pluginapi.HostAuthSaveRequest
	callHostCallback = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthGet:
			return json.Marshal(pluginapi.HostAuthGetResponse{AuthIndex: "idx-a", Name: "codex-a.json", JSON: original})
		case pluginabi.MethodHostAuthSave:
			saved = payload.(pluginapi.HostAuthSaveRequest)
			return json.Marshal(pluginapi.HostAuthSaveResponse{Name: saved.Name})
		default:
			t.Fatalf("unexpected host callback method %q", method)
			return nil, nil
		}
	}

	if err := (ABIHostClient{}).SetAuthDisabled("idx-a", true); err != nil {
		t.Fatal(err)
	}
	if saved.Name != "codex-a.json" {
		t.Fatalf("saved name=%q, want original physical file name", saved.Name)
	}
	var before, after map[string]any
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(saved.JSON, &after); err != nil {
		t.Fatal(err)
	}
	if disabled, ok := after["disabled"].(bool); !ok || !disabled {
		t.Fatal("saved auth did not set disabled=true")
	}
	delete(after, "disabled")
	beforeRaw, _ := json.Marshal(before)
	afterRaw, _ := json.Marshal(after)
	if string(beforeRaw) != string(afterRaw) {
		t.Fatal("SetAuthDisabled changed credential fields other than disabled")
	}
}
