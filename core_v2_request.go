package main

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const codexProbeUserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"

func coreProbeRequest(credentials CodexCredentials) pluginapi.HTTPRequest {
	return pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    codexResetProbeEndpoint,
		Headers: http.Header{
			"Accept":             []string{"text/event-stream"},
			"Authorization":      []string{"Bearer " + credentials.AccessToken},
			"Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID},
			"Content-Type":       []string{"application/json"},
			"OpenAI-Beta":        []string{"responses=v1"},
			"originator":         []string{"codex_cli_rs"},
			"User-Agent":         []string{codexProbeUserAgent},
		},
		Body: resetProbePayloadBytes(),
	}
}
