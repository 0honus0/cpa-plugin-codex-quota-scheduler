package main

const (
	codexResetProbeEndpoint = "https://chatgpt.com/backend-api/codex/responses"
	resetProbePayload       = `{"model":"gpt-5.4-mini","instructions":"You are a helpful assistant.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}],"store":false,"stream":true}`
)

func resetProbePayloadBytes() []byte {
	return []byte(resetProbePayload)
}
