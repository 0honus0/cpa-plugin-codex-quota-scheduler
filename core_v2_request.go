package main

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// coreProbeRequest intentionally owns no payload definition. The scheduler
// reuses the existing probe request content byte-for-byte and changes only the
// scheduling/refresh decision that determines when it is sent.
func coreProbeRequest(credentials CodexCredentials) pluginapi.HTTPRequest {
	return pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    codexResetProbeEndpoint,
		Headers: http.Header{
			"Authorization":      []string{"Bearer " + credentials.AccessToken},
			"Chatgpt-Account-Id": []string{credentials.ChatGPTAccountID},
			"Content-Type":       []string{"application/json"},
		},
		Body: resetProbePayloadBytes(),
	}
}
