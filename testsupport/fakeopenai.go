package testsupport

import (
	"context"
	"io"
	"net/http"
	"sync"
)

type HTTPResult struct {
	StatusCode int
	Body       []byte
	Err        error
}
type HTTPCall struct{ Method, URL string }
type FakeOpenAI struct {
	mu        sync.Mutex
	Responses []HTTPResult
	calls     []HTTPCall
}

func (f *FakeOpenAI) Do(_ context.Context, method, url string, _ http.Header, _ io.Reader) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, HTTPCall{method, url})
	r := f.Responses[0]
	f.Responses = f.Responses[1:]
	if r.Err != nil {
		return nil, r.Err
	}
	return &http.Response{StatusCode: r.StatusCode, Body: io.NopCloser(&byteReader{b: r.Body})}, nil
}
func (f *FakeOpenAI) Calls() []HTTPCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HTTPCall(nil), f.calls...)
}

type byteReader struct{ b []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
