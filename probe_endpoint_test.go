package main

import "testing"

func TestCodexResetProbeUsesResponsesEndpoint(t *testing.T) {
	const want = "https://chatgpt.com/backend-api/codex/responses"
	if codexResetProbeEndpoint != want {
		t.Fatalf("codexResetProbeEndpoint = %q, want %q", codexResetProbeEndpoint, want)
	}
}
