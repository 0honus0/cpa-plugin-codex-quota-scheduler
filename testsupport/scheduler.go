package testsupport

import "sync"

type Event struct {
	Name string
	Run  func()
}
type EventScheduler struct {
	mu     sync.Mutex
	max    int
	events []Event
}

func NewEventScheduler(max int) *EventScheduler { return &EventScheduler{max: max} }
func (s *EventScheduler) Queue(name string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, Event{Name: name, Run: fn})
}
func (s *EventScheduler) Interleavings() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.events)
	if n > s.max {
		n = s.max
	}
	src := s.events[:n]
	var out [][]string
	var walk func([]Event, []string)
	walk = func(left []Event, prefix []string) {
		if len(left) == 0 {
			out = append(out, append([]string(nil), prefix...))
			return
		}
		for i, e := range left {
			next := append([]Event(nil), left[:i]...)
			next = append(next, left[i+1:]...)
			walk(next, append(prefix, e.Name))
		}
	}
	walk(src, nil)
	return out
}
