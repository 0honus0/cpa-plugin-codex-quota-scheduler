package testsupport

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestFakeHostAndOpenAIRecordScriptedCalls(t *testing.T) {
	host := &FakeHost{SaveResults: []error{errors.New("save failed")}}
	if err := host.SaveAuth(context.Background(), "auth", []byte("value")); err == nil {
		t.Fatal("SaveAuth succeeded")
	}
	if len(host.Calls()) != 1 {
		t.Fatalf("host calls = %v", host.Calls())
	}
	openai := &FakeOpenAI{Responses: []HTTPResult{{StatusCode: 202, Body: []byte("ok")}}}
	resp, err := openai.Do(context.Background(), "POST", "https://example.invalid", nil, nil)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("response=%v err=%v", resp, err)
	}
	if len(openai.Calls()) != 1 {
		t.Fatalf("openai calls = %v", openai.Calls())
	}
}

func TestCrashControllerUsesRegisteredKPoints(t *testing.T) {
	registry := NewKPointRegistry("K_WRITE")
	crash := NewCrashController(registry, "K_WRITE")
	if err := crash.Hit("K_WRITE"); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("Hit error = %v", err)
	}
	if err := crash.Hit("K_UNKNOWN"); err == nil {
		t.Fatal("unknown point accepted")
	}
}
