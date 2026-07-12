package testsupport

import (
	"context"
	"errors"
	"net/http"
	"sync"
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

func TestFakeOpenAIEmptyScriptReturnsError(t *testing.T) {
	if _, err := new(FakeOpenAI).Do(context.Background(), "GET", "x", nil, nil); err == nil {
		t.Fatal("empty script panicked or succeeded")
	}
}

func TestCrashControllerAndSchedulerConcurrentUse(t *testing.T) {
	r := NewKPointRegistry("K")
	c := NewCrashController(r, "K")
	s := NewEventScheduler(3)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.Hit("K"); s.Queue("x", func() {}) }()
	}
	wg.Wait()
	_ = s.Interleavings()
}
