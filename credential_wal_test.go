package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeffery/codex-quota-scheduler/testsupport"
)

type walStore struct {
	states []PersistentState
	fail   error
}

func (s *walStore) WriteThrough(st PersistentState) error {
	s.states = append(s.states, st)
	return s.fail
}

type walHost struct {
	current HostAuth
	saveErr error
	calls   int
}

func crashController(point string) *testsupport.CrashController {
	return testsupport.NewCrashController(testsupport.NewKPointRegistry("K_CREDENTIAL_PLANNED_WRITE", "K_CREDENTIAL_AFTER_PLANNED", "K_CREDENTIAL_BEFORE_SAVEAUTH", "K_CREDENTIAL_AFTER_SAVEAUTH", "K_CREDENTIAL_BEFORE_TERMINAL", "K_CREDENTIAL_TERMINAL_WRITE", "K_FENCE_CEILING_WRITE", "K_FENCE_AFTER_CEILING", "K_STATE_BEFORE_RENAME", "K_STATE_AFTER_RENAME"), point)
}
func mustCredentialManager(t *testing.T, store StateWriter, host CredentialHost, now func() time.Time, crash CrashHitter) *CredentialManager {
	t.Helper()
	m, err := NewCredentialManager(store, host, now, crash)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (h *walHost) SaveAuth(_ context.Context, _ AuthInstanceID, next HostAuth) error {
	h.calls++
	if h.saveErr == nil {
		h.current = next
	}
	return h.saveErr
}
func (h *walHost) GetAuth(context.Context, AuthInstanceID) (HostAuth, error) { return h.current, nil }

func TestCredentialSaveWALOrdering(t *testing.T) {
	prev := HostAuth{Fingerprint: fp("s", "r0", "m")}
	next := HostAuth{Fingerprint: fp("s", "r1", "m")}
	store := &walStore{}
	host := &walHost{current: prev}
	m := mustCredentialManager(t, store, host, func() time.Time { return time.Unix(1, 0) }, nil)
	m.SetChain(7, TransitionChain{Cursor: prev.Fingerprint})
	result, err := m.SaveVersioned(context.Background(), 7, next, ExecutionToken{Instance: 7})
	if err != nil || result.Phase != TransitionApplied {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(store.states) != 3 || store.states[1].CredentialChains[7].Transitions[0].Phase != TransitionPlanned || store.states[2].CredentialChains[7].Transitions[0].Phase != TransitionApplied {
		t.Fatalf("states=%+v", store.states)
	}
}

func TestCredentialSaveFailureAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want TransitionPhase
	}{{"failure", ErrCredentialRejected, TransitionAborted}, {"unknown", ErrCredentialOutcomeUnknown, TransitionOutcomeUnknown}} {
		t.Run(tc.name, func(t *testing.T) {
			prev := HostAuth{Fingerprint: fp("s", "r0", "m")}
			store := &walStore{}
			host := &walHost{current: prev, saveErr: tc.err}
			m := mustCredentialManager(t, store, host, time.Now, nil)
			m.SetChain(1, TransitionChain{Cursor: prev.Fingerprint})
			got, _ := m.SaveVersioned(context.Background(), 1, HostAuth{Fingerprint: fp("s", "r1", "m")}, ExecutionToken{Instance: 1})
			if got.Phase != tc.want {
				t.Fatalf("phase=%s", got.Phase)
			}
		})
	}
}

func TestCredentialContextAndTransportErrorsAreUnknown(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, ErrCredentialOutcomeUnknown} {
		store := &walStore{}
		prev := HostAuth{Fingerprint: fp("s", "0", "m")}
		m := mustCredentialManager(t, store, &walHost{current: prev, saveErr: err}, time.Now, nil)
		m.SetChain(1, TransitionChain{Cursor: prev.Fingerprint})
		got, _ := m.SaveVersioned(context.Background(), 1, HostAuth{Fingerprint: fp("s", "1", "m")}, ExecutionToken{Instance: 1})
		if got.Phase != TransitionOutcomeUnknown {
			t.Fatalf("%v phase=%s", err, got.Phase)
		}
	}
}

func TestCredentialKnownRejectionRetryUsesAppliedTail(t *testing.T) {
	prev := HostAuth{Fingerprint: fp("s", "0", "m")}
	store := &walStore{}
	host := &walHost{current: prev, saveErr: ErrCredentialRejected}
	m := mustCredentialManager(t, store, host, time.Now, nil)
	m.SetChain(1, TransitionChain{Cursor: prev.Fingerprint})
	m.SaveVersioned(context.Background(), 1, HostAuth{Fingerprint: fp("s", "rejected", "m")}, ExecutionToken{Instance: 1})
	host.saveErr = nil
	m.SaveVersioned(context.Background(), 1, HostAuth{Fingerprint: fp("s", "retry", "m")}, ExecutionToken{Instance: 1})
	chain := m.Chain(1)
	if chain.Transitions[1].Prev != prev.Fingerprint {
		t.Fatalf("retry prev used rejected next: %+v", chain)
	}
}

func TestCredentialUnknownBlocksRetryUntilReconciled(t *testing.T) {
	prev := HostAuth{Fingerprint: fp("s", "0", "m")}
	store := &walStore{}
	host := &walHost{current: prev, saveErr: context.DeadlineExceeded}
	m := mustCredentialManager(t, store, host, time.Now, nil)
	m.SetChain(1, TransitionChain{Cursor: prev.Fingerprint})
	m.SaveVersioned(context.Background(), 1, HostAuth{Fingerprint: fp("s", "unknown", "m")}, ExecutionToken{Instance: 1})
	host.saveErr = nil
	if _, err := m.SaveVersioned(context.Background(), 1, HostAuth{Fingerprint: fp("s", "retry", "m")}, ExecutionToken{Instance: 1}); !errors.Is(err, ErrCredentialUnresolved) {
		t.Fatalf("err=%v", err)
	}
}

func TestSetChainFailureDoesNotMutateMemory(t *testing.T) {
	store := &walStore{fail: errors.New("disk")}
	m := mustCredentialManager(t, store, &walHost{}, time.Now, nil)
	err := m.SetChain(1, TransitionChain{Cursor: fp("s", "r", "m")})
	if err == nil {
		t.Fatal("SetChain ignored error")
	}
	if m.Chain(1).GenerationCount() != 1 || m.Chain(1).Cursor != (CredentialFingerprint{}) {
		t.Fatal("memory mutated before durability")
	}
}
func TestCredentialManagerConstructorSurfacesLoadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	hooks := OSFileHooks()
	hooks.ReadFile = func(string) ([]byte, error) { return nil, os.ErrPermission }
	store := NewStateStore(path, hooks, nil)
	if _, err := NewCredentialManager(store, &walHost{}, time.Now, nil); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err=%v", err)
	}
}

func TestCredentialPlannedWriteFailureRollsBackMemory(t *testing.T) {
	prev := HostAuth{Fingerprint: fp("s", "0", "m")}
	store := &walStore{}
	host := &walHost{current: prev}
	m := mustCredentialManager(t, store, host, time.Now, nil)
	m.SetChain(1, TransitionChain{Cursor: prev.Fingerprint})
	store.fail = errors.New("disk")
	m.SaveVersioned(context.Background(), 1, HostAuth{Fingerprint: fp("s", "1", "m")}, ExecutionToken{Instance: 1})
	store.fail = nil
	got, err := m.SaveVersioned(context.Background(), 1, HostAuth{Fingerprint: fp("s", "2", "m")}, ExecutionToken{Instance: 1})
	if err != nil || got.SaveSeq != 1 || m.Chain(1).Transitions[0].Prev != prev.Fingerprint {
		t.Fatalf("got=%+v chain=%+v err=%v", got, m.Chain(1), err)
	}
}

func TestCredentialRecoveryReconcilesUnknownOutcome(t *testing.T) {
	prev := HostAuth{Fingerprint: fp("s", "r0", "m")}
	next := HostAuth{Fingerprint: fp("s", "r1", "m")}
	for _, tc := range []struct {
		name    string
		current HostAuth
		want    TransitionPhase
		amb     bool
	}{{"applied", next, TransitionApplied, false}, {"aborted", prev, TransitionAborted, false}, {"ambiguous", HostAuth{Fingerprint: fp("x", "y", "z")}, TransitionOutcomeUnknown, true}} {
		t.Run(tc.name, func(t *testing.T) {
			tr := CredentialTransition{Prev: prev.Fingerprint, Next: next.Fingerprint, Phase: TransitionOutcomeUnknown}
			store := &walStore{}
			host := &walHost{current: tc.current}
			m := mustCredentialManager(t, store, host, time.Now, nil)
			m.SetChain(1, TransitionChain{Cursor: prev.Fingerprint, Transitions: []CredentialTransition{tr}})
			report, err := m.Reconcile(context.Background(), 1)
			if err != nil || report.Ambiguous != tc.amb || m.Chain(1).Transitions[0].Phase != tc.want {
				t.Fatalf("report=%+v phase=%s err=%v", report, m.Chain(1).Transitions[0].Phase, err)
			}
		})
	}
}

func TestCredentialSaveRejectsOldInstance(t *testing.T) {
	m := mustCredentialManager(t, &walStore{}, &walHost{}, time.Now, nil)
	m.SetChain(2, TransitionChain{Cursor: fp("s", "r", "m")})
	_, err := m.SaveVersioned(context.Background(), 2, HostAuth{Fingerprint: fp("s", "r2", "m")}, ExecutionToken{Instance: 1})
	if !errors.Is(err, ErrStaleExecutionToken) {
		t.Fatalf("err=%v", err)
	}
}

func TestCredentialCrashLeavesRecoverableWAL(t *testing.T) {
	prev := HostAuth{Fingerprint: fp("s", "r0", "m")}
	next := HostAuth{Fingerprint: fp("s", "r1", "m")}
	for _, point := range []string{"K_CREDENTIAL_AFTER_PLANNED", "K_CREDENTIAL_BEFORE_TERMINAL"} {
		t.Run(point, func(t *testing.T) {
			store := &walStore{}
			host := &walHost{current: prev}
			m := mustCredentialManager(t, store, host, time.Now, crashController(point))
			m.SetChain(1, TransitionChain{Cursor: prev.Fingerprint})
			_, err := m.SaveVersioned(context.Background(), 1, next, ExecutionToken{Instance: 1})
			if err == nil {
				t.Fatal("crash not injected")
			}
			last := store.states[len(store.states)-1]
			if len(store.states) == 0 || len(last.CredentialChains[1].Transitions) == 0 || last.CredentialChains[1].Transitions[0].Phase != TransitionPlanned {
				t.Fatalf("persisted=%+v", store.states)
			}
		})
	}
}

func TestFreshManagerReconcilesCrashAfterSaveAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	prev := HostAuth{Fingerprint: fp("s", "0", "m")}
	next := HostAuth{Fingerprint: fp("s", "1", "m")}
	host := &walHost{current: prev}
	m := mustCredentialManager(t, store, host, time.Now, crashController("K_CREDENTIAL_AFTER_SAVEAUTH"))
	m.SetChain(1, TransitionChain{Cursor: prev.Fingerprint})
	if _, err := m.SaveVersioned(context.Background(), 1, next, ExecutionToken{Instance: 1}); err == nil {
		t.Fatal("crash missing")
	}
	freshStore := NewStateStore(path, OSFileHooks(), nil)
	fresh := mustCredentialManager(t, freshStore, host, time.Now, nil)
	report, err := fresh.Reconcile(context.Background(), 1)
	if err != nil || report.Phase != TransitionApplied {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}
