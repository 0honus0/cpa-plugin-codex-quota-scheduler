package main

import "time"

const ProbeSource = "probe"
const (
	OperationProbeRead OperationClass = "quota_read"
	OperationProbeSend OperationClass = "probe_send"
)

type ProbeWAL struct {
	store *StateStore
	crash CrashHitter
}

func NewProbeWAL(s *StateStore, crash ...CrashHitter) *ProbeWAL {
	w := &ProbeWAL{store: s}
	if len(crash) > 0 {
		w.crash = crash[0]
	}
	return w
}
func (w *ProbeWAL) hit(name string) error {
	if w.crash != nil {
		return w.crash.Hit(name)
	}
	return nil
}
func (w *ProbeWAL) PersistSending(a ProbeAttempt) error {
	a.Phase = ProbeAttemptSending
	if a.SuppressUntil.IsZero() {
		a.SuppressUntil = a.CreatedAt.Add(10 * time.Minute)
	}
	//kpoint:K_PROBE_SENDING_WRITE
	if err := w.hit("K_PROBE_SENDING_WRITE"); err != nil {
		return err
	}
	_, err := w.store.Update(func(s *PersistentState) error { s.ProbeAttempts[a.Instance] = a; return nil })
	//kpoint:K_PROBE_AFTER_SENDING
	if err == nil {
		err = w.hit("K_PROBE_AFTER_SENDING")
	}
	return err
}
func (w *ProbeWAL) ExecuteSend(send func() error) error { //kpoint:K_PROBE_BEFORE_HTTP
	if err := w.hit("K_PROBE_BEFORE_HTTP"); err != nil {
		return err
	}
	err := send()
	if err != nil {
		return err
	} //kpoint:K_PROBE_AFTER_HTTP
	return w.hit("K_PROBE_AFTER_HTTP")
}
func (w *ProbeWAL) PersistSent(i AuthInstanceID, at time.Time) error {
	//kpoint:K_PROBE_SENT_WRITE
	if err := w.hit("K_PROBE_SENT_WRITE"); err != nil {
		return err
	}
	_, err := w.store.Update(func(s *PersistentState) error {
		a := s.ProbeAttempts[i]
		a.Phase = ProbeAttemptSent
		a.SentAt = &at
		s.ProbeAttempts[i] = a
		return nil
	})
	return err
}
func (w *ProbeWAL) Recover(now time.Time) []Intent {
	st, err := w.store.PersistentSnapshot()
	if err != nil {
		return nil
	}
	var out []Intent
	for i, a := range st.ProbeAttempts {
		if a.Phase != ProbeAttemptSending && a.Phase != ProbeAttemptSent && a.Phase != ProbeAttemptSentUnknown {
			continue
		}
		if now.Before(a.VerifyNotBefore) {
			continue
		}
		if a.Phase == ProbeAttemptSending {
			a.Phase = ProbeAttemptSentUnknown
			_, _ = w.store.Update(func(s *PersistentState) error { s.ProbeAttempts[i] = a; return nil })
		}
		out = append(out, Intent{Instance: i, Class: OperationProbeRead, Source: ProbeSource, StartedAfter: a.SendFenceSeq, Payload: a.Windows})
	}
	return out
}
