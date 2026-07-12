package main

import (
	"fmt"
	"sync"
	"time"
)

type ProbeWindowState string

const (
	ProbeIdle               ProbeWindowState = "Idle"
	ProbeWaitingReset       ProbeWindowState = "WaitingReset"
	ProbePendingCheck       ProbeWindowState = "PendingCheck"
	ProbeSentAwaitingVerify ProbeWindowState = "SentAwaitingVerify"
	ProbeSentUnknown        ProbeWindowState = "SentUnknown"
	ProbeRetryWait          ProbeWindowState = "RetryWait"
	ProbeConfirmed          ProbeWindowState = "Confirmed"
	ProbeAuthBlocked        ProbeWindowState = "AuthBlocked"
	ProbeAnomalyHold        ProbeWindowState = "AnomalyHold"
	ProbeWaitingRoster      ProbeWindowState = "WaitingRoster"
)

type ProbeWindow struct {
	State      ProbeWindowState `json:"state"`
	Baseline   ProbeBaseline    `json:"baseline"`
	Deadline   time.Time        `json:"deadline,omitempty"`
	RetryCount int              `json:"retry_count,omitempty"`
	AttemptID  string           `json:"attempt_id,omitempty"`
}
type ProbeEventKind string

const (
	ProbeEventDeadline        ProbeEventKind = "deadline"
	ProbeEventPrecheckResult  ProbeEventKind = "precheck_result"
	ProbeEventVerifyResult    ProbeEventKind = "verify_result"
	ProbeEventAuthFailed      ProbeEventKind = "auth_failed"
	ProbeEventExternalLogin   ProbeEventKind = "external_login"
	ProbeEventRosterConfirmed ProbeEventKind = "roster_confirmed"
	ProbeEventInstanceRemoved ProbeEventKind = "instance_removed"
)

type ProbeEvent struct {
	Kind        ProbeEventKind
	Window      ProbeWindowKind
	Now         time.Time
	Snapshots   map[ProbeWindowKind]QuotaSnapshot
	RefreshMode RefreshMode
}
type ProbeController struct {
	mu      sync.Mutex
	now     time.Time
	windows map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow
	seq     uint64
}

func NewProbeController(now time.Time) *ProbeController {
	return &ProbeController{now: now, windows: map[AuthInstanceID]map[ProbeWindowKind]ProbeWindow{}}
}
func (c *ProbeController) SetWindow(i AuthInstanceID, k ProbeWindowKind, w ProbeWindow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.windows[i] == nil {
		c.windows[i] = map[ProbeWindowKind]ProbeWindow{}
	}
	c.windows[i][k] = w
}
func (c *ProbeController) Window(i AuthInstanceID, k ProbeWindowKind) (ProbeWindow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.windows[i][k]
	return w, ok
}
func (c *ProbeController) Advance(i AuthInstanceID, e ProbeEvent) []Intent {
	c.mu.Lock()
	defer c.mu.Unlock()
	ws := c.windows[i]
	if ws == nil {
		return nil
	}
	if e.Kind == ProbeEventInstanceRemoved {
		delete(c.windows, i)
		return nil
	}
	var out []Intent
	for k, w := range ws {
		if e.Window != "" && e.Window != k {
			continue
		}
		switch e.Kind {
		case ProbeEventDeadline:
			if (w.State == ProbeWaitingReset || w.State == ProbeRetryWait || w.State == ProbeAnomalyHold) && !w.Deadline.After(e.Now) {
				w.State = ProbePendingCheck
				ws[k] = w
				out = append(out, Intent{Instance: i, Class: OperationProbeRead, Source: ProbeSource, Payload: []ProbeWindowKind{k}})
			}
		case ProbeEventPrecheckResult, ProbeEventVerifyResult:
			if (e.Kind == ProbeEventPrecheckResult && w.State != ProbePendingCheck) || (e.Kind == ProbeEventVerifyResult && w.State != ProbeSentAwaitingVerify && w.State != ProbeSentUnknown) {
				continue
			}
			snap, ok := e.Snapshots[k]
			if !ok {
				continue
			}
			cl := ClassifyProbeWindow(w.Baseline, snap, e.Now)
			w.Baseline = cl.Baseline
			switch cl.Kind {
			case ProbeActivatedNew, ProbeActivatedInferred:
				w.State = ProbeConfirmed
				w.Deadline = time.Time{}
			case ProbeNotDueYet:
				w.State = ProbeWaitingReset
				w.Deadline = deadlineFor(w.Baseline, e.Now)
			case ProbeAnomaly:
				w.State = ProbeAnomalyHold
				w.Deadline = e.Now.Add(probeUnknownResetRecheck)
			case ProbeStillLazy, ProbeAmbiguous:
				if e.Kind == ProbeEventPrecheckResult {
					c.seq++
					w.State = ProbeSentAwaitingVerify
					w.AttemptID = fmt.Sprintf("probe-%d", c.seq)
					out = append(out, Intent{Instance: i, Class: OperationProbeSend, Source: ProbeSource, Payload: []ProbeWindowKind{k}})
				} else {
					w.State = ProbeRetryWait
					w.RetryCount++
					w.Deadline = e.Now.Add(probeBackoff(w.RetryCount))
				}
			default:
				w.State = ProbeRetryWait
				w.Deadline = e.Now.Add(probeBackoff(w.RetryCount))
			}
			ws[k] = w
		case ProbeEventAuthFailed:
			if w.State != ProbeIdle {
				w.State = ProbeAuthBlocked
				ws[k] = w
			}
		case ProbeEventExternalLogin:
			if w.State == ProbeAuthBlocked {
				w.State = ProbePendingCheck
				ws[k] = w
			}
		case ProbeEventRosterConfirmed:
			if w.State == ProbeWaitingRoster {
				w.State = ProbeWaitingReset
				w.Deadline = deadlineFor(w.Baseline, e.Now)
				ws[k] = w
			}
		}
	}
	return out
}
func deadlineFor(b ProbeBaseline, now time.Time) time.Time {
	if b.Kind == ProbeBaselineUsageOnly {
		return b.NextRecheckAt
	}
	if b.ResetAt.IsZero() {
		return now
	}
	return b.ResetAt.Add(probeRefreshAfterResetDelay)
}
func probeBackoff(n int) time.Duration {
	if n <= 1 {
		return time.Minute
	}
	if n == 2 {
		return 5 * time.Minute
	}
	return 15 * time.Minute
}
