package testsupport

import (
	"context"
	"errors"
	"sync"
)

type HostCall struct {
	Method, Name string
	Value        []byte
}
type FakeHost struct {
	mu          sync.Mutex
	SaveResults []error
	AuthValues  [][]byte
	calls       []HostCall
}

func (f *FakeHost) SaveAuth(_ context.Context, name string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, HostCall{Method: "SaveAuth", Name: name, Value: append([]byte(nil), value...)})
	if len(f.SaveResults) == 0 {
		return nil
	}
	e := f.SaveResults[0]
	f.SaveResults = f.SaveResults[1:]
	return e
}
func (f *FakeHost) GetAuth(_ context.Context, name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, HostCall{Method: "GetAuth", Name: name})
	if len(f.AuthValues) == 0 {
		return nil, errors.New("no scripted auth")
	}
	v := f.AuthValues[0]
	f.AuthValues = f.AuthValues[1:]
	return append([]byte(nil), v...), nil
}
func (f *FakeHost) Calls() []HostCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HostCall(nil), f.calls...)
}
