package main

import (
	"errors"
	"github.com/jeffery/codex-quota-scheduler/testsupport"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"
)

func TestProbeWALSendingRecoversVerifyFirstAndSuppresses(t *testing.T) { //inv:INV-27,INV-36,INV-38
	now := time.Unix(3000, 0).UTC()
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	wal := NewProbeWAL(store)
	a := ProbeAttempt{Instance: 1, AttemptID: "a", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, Phase: ProbeAttemptSending, SendFenceSeq: 7, CreatedAt: now, VerifyNotBefore: now.Add(3 * time.Second)}
	if err := wal.PersistSending(a); err != nil {
		t.Fatal(err)
	}
	got := wal.Recover(now.Add(4 * time.Second))
	if len(got) != 1 || got[0].Class != OperationProbeRead || got[0].StartedAfter != 7 {
		t.Fatalf("recovery=%v", got)
	}
	state, _ := store.PersistentSnapshot()
	if state.ProbeAttempts[1].Phase != ProbeAttemptSentUnknown || !state.ProbeAttempts[1].SuppressUntil.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("attempt=%#v", state.ProbeAttempts[1])
	}
}

func TestS6KPointRegistryMatchesSource(t *testing.T) {
	raw, err := os.ReadFile("probe_wal.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`kpoint:(K_PROBE_[A-Z_]+)`)
	m := re.FindAllStringSubmatch(string(raw), -1)
	var got []string
	for _, x := range m {
		got = append(got, x[1])
	}
	sort.Strings(got)
	want := []string{"K_PROBE_AFTER_HTTP", "K_PROBE_AFTER_SENDING", "K_PROBE_BEFORE_HTTP", "K_PROBE_SENDING_WRITE", "K_PROBE_SENT_WRITE"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("source=%v registry=%v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("source=%v registry=%v", got, want)
		}
	}
}

func TestProbeKPointsReachableAndRegistered(t *testing.T) { //inv:INV-27,INV-38
	points := []string{"K_PROBE_SENDING_WRITE", "K_PROBE_AFTER_SENDING", "K_PROBE_BEFORE_HTTP", "K_PROBE_AFTER_HTTP", "K_PROBE_SENT_WRITE"}
	now := time.Unix(5000, 0).UTC()
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			registry := testsupport.NewKPointRegistry(points...)
			crash := testsupport.NewCrashController(registry, point)
			store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
			wal := NewProbeWAL(store, crash)
			a := ProbeAttempt{Instance: 1, AttemptID: "k", Windows: []ProbeWindowKind{ProbeWindowFiveHour}, CreatedAt: now, VerifyNotBefore: now}
			var err error
			switch point {
			case "K_PROBE_SENDING_WRITE", "K_PROBE_AFTER_SENDING":
				err = wal.PersistSending(a)
			case "K_PROBE_BEFORE_HTTP", "K_PROBE_AFTER_HTTP":
				err = wal.ExecuteSend(func() error { return nil })
			case "K_PROBE_SENT_WRITE":
				_ = NewProbeWAL(store).PersistSending(a)
				err = wal.PersistSent(1, now)
			}
			if !errors.Is(err, testsupport.ErrInjectedCrash) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestMockGroupAProbeRecovery(t *testing.T) {
	t.Run("sending", TestProbeWALSendingRecoversVerifyFirstAndSuppresses)
}

func TestProbeRecoveryWaitsForGraceAndNeverResendsDuringSuppression(t *testing.T) { //inv:INV-27,INV-36
	now := time.Unix(4000, 0).UTC()
	store := NewStateStore(filepath.Join(t.TempDir(), "state.json"), OSFileHooks(), nil)
	wal := NewProbeWAL(store)
	a := ProbeAttempt{Instance: 3, AttemptID: "jump", Windows: []ProbeWindowKind{ProbeWindowLong}, Phase: ProbeAttemptSent, SendFenceSeq: 11, CreatedAt: now, VerifyNotBefore: now.Add(3 * time.Second), SuppressUntil: now.Add(10 * time.Minute)}
	if err := wal.PersistSending(a); err != nil {
		t.Fatal(err)
	}
	if got := wal.Recover(now.Add(2 * time.Second)); len(got) != 0 {
		t.Fatalf("early recovery=%v", got)
	}
	got := wal.Recover(now.Add(3 * time.Second))
	if len(got) != 1 || got[0].Class == OperationProbeSend {
		t.Fatalf("recovery=%v", got)
	}
}
