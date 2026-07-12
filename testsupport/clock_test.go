package testsupport

import (
	"reflect"
	"testing"
	"time"
)

func TestClockDeliversDueTimersDeterministically(t *testing.T) {
	start := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	clock := NewClock(start)
	var got []string
	clock.AfterFunc(2*time.Second, func() { got = append(got, "second") })
	clock.AfterFunc(time.Second, func() { got = append(got, "first") })
	clock.Advance(2 * time.Second)
	if !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("delivery order = %v", got)
	}
}

func TestEventSchedulerEnumeratesBoundedInterleavings(t *testing.T) {
	s := NewEventScheduler(3)
	s.Queue("a", func() {})
	s.Queue("b", func() {})
	orders := s.Interleavings()
	if len(orders) != 2 {
		t.Fatalf("orders = %v", orders)
	}
}
