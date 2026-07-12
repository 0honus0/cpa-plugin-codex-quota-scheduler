package main

import "time"

type ProbeWindowKind string

const (
	ProbeWindowFiveHour ProbeWindowKind = "five_hour"
	ProbeWindowLong     ProbeWindowKind = "long"
)

type ProbeAttemptPhase string

const (
	ProbeAttemptPrepared    ProbeAttemptPhase = "prepared"
	ProbeAttemptSentUnknown ProbeAttemptPhase = "sent_unknown"
)

// ProbeAttemptSeam is storage-only until S6 owns probe transitions.
type ProbeAttemptSeam struct {
	Instance        AuthInstanceID    `json:"instance"`
	AttemptID       string            `json:"attempt_id"`
	Windows         []ProbeWindowKind `json:"windows"`
	Phase           ProbeAttemptPhase `json:"phase"`
	SendFenceSeq    uint64            `json:"send_fence_seq"`
	CreatedAt       time.Time         `json:"created_at"`
	SentAt          *time.Time        `json:"sent_at,omitempty"`
	VerifyNotBefore time.Time         `json:"verify_not_before"`
	SuppressUntil   time.Time         `json:"suppress_until"`
}
