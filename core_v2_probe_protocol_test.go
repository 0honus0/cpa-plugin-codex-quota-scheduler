package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCoreProbeRequestMatchesCodexResponsesProtocol(t *testing.T) {
	credentials := CodexCredentials{AccessToken: "access", ChatGPTAccountID: "acct"}
	req := coreProbeRequest(credentials)

	if req.URL != codexResetProbeEndpoint {
		t.Fatalf("probe URL=%q, want %q", req.URL, codexResetProbeEndpoint)
	}
	if got := req.Headers.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept=%q, want text/event-stream", got)
	}
	if got := req.Headers.Get("OpenAI-Beta"); got != "responses=v1" {
		t.Fatalf("OpenAI-Beta=%q, want responses=v1", got)
	}
	if got := req.Headers.Get("originator"); got != "codex_cli_rs" {
		t.Fatalf("originator=%q, want codex_cli_rs", got)
	}
	if got := req.Headers.Get("User-Agent"); got != codexProbeUserAgent {
		t.Fatalf("User-Agent=%q, want %q", got, codexProbeUserAgent)
	}
	if got := req.Headers.Get("Authorization"); got != "Bearer access" {
		t.Fatalf("Authorization=%q", got)
	}
	if got := req.Headers.Get("Chatgpt-Account-Id"); got != "acct" {
		t.Fatalf("Chatgpt-Account-Id=%q", got)
	}

	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("decode probe body: %v", err)
	}
	if body["store"] != false || body["stream"] != true {
		t.Fatalf("probe body store/stream=%v/%v, want false/true", body["store"], body["stream"])
	}
	if body["model"] != "gpt-5.4-mini" {
		t.Fatalf("model=%v", body["model"])
	}
	if !bytes.Contains(req.Body, []byte(`"type":"input_text"`)) || !bytes.Contains(req.Body, []byte(`"text":"ping"`)) {
		t.Fatalf("probe body does not contain expected input_text ping: %s", req.Body)
	}
}
