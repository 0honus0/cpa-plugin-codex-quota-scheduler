package main

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const codexProbeUserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"

func coreProbeRequest(credentials CodexCredentials) pluginapi.HTTPRequest {
	headers := make(http.Header)
	headers.Set("Accept", "text/event-stream")
	headers.Set("Authorization", "Bearer "+credentials.AccessToken)
	headers.Set("Chatgpt-Account-Id", credentials.ChatGPTAccountID)
	headers.Set("Content-Type", "application/json")
	headers.Set("OpenAI-Beta", "responses=v1")
	headers.Set("originator", "codex_cli_rs")
	headers.Set("User-Agent", codexProbeUserAgent)

	return pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     codexResetProbeEndpoint,
		Headers: headers,
		Body:    resetProbePayloadBytes(),
	}
}
