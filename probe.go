package main

const (
	codexResetProbeEndpoint = "https://chatgpt.com/backend-api/codex/responses"
	resetProbePayload       = `{"model":"gpt-5.4-mini","instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}]}`
)

func resetProbePayloadBytes() []byte {
	return []byte(resetProbePayload)
}
