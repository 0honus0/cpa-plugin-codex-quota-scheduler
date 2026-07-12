package main

import (
	"crypto/sha256"
	"time"
)

type AccountIdentity uint64
type AuthInstanceID uint64
type InstanceAdmissionEpoch uint64
type TierGeneration uint64
type AuthBindingEpoch uint64
type LoginEpoch uint64
type TokenEpoch uint64

type ExecutionToken struct {
	Instance  AuthInstanceID         `json:"instance"`
	Admission InstanceAdmissionEpoch `json:"admission"`
	Tier      TierGeneration         `json:"tier"`
	Fence     uint64                 `json:"fence"`
}

func (t ExecutionToken) ValidFor(instance AuthInstanceID, admission InstanceAdmissionEpoch, tier TierGeneration) bool {
	return t.Instance == instance && t.Admission == admission && t.Tier == tier
}

type CredentialFingerprint struct {
	SubjectHash      [32]byte `json:"subject_hash"`
	RefreshTokenHash [32]byte `json:"refresh_token_hash"`
	MetadataHash     [32]byte `json:"metadata_hash"`
	CompositeHash    [32]byte `json:"composite_hash"`
}

func NewCredentialFingerprint(subject, refresh, metadata string) CredentialFingerprint {
	s := sha256.Sum256([]byte(subject))
	r := sha256.Sum256([]byte(refresh))
	m := sha256.Sum256([]byte(metadata))
	b := make([]byte, 0, 96)
	b = append(b, s[:]...)
	b = append(b, r[:]...)
	b = append(b, m[:]...)
	return CredentialFingerprint{s, r, m, sha256.Sum256(b)}
}

type TransitionPhase string

const (
	TransitionPlanned        TransitionPhase = "planned"
	TransitionApplied        TransitionPhase = "applied"
	TransitionAborted        TransitionPhase = "aborted"
	TransitionOutcomeUnknown TransitionPhase = "outcome_unknown"
)

type CredentialTransition struct {
	Prev      CredentialFingerprint `json:"prev"`
	Next      CredentialFingerprint `json:"next"`
	SaveSeq   uint64                `json:"save_seq"`
	Phase     TransitionPhase       `json:"phase"`
	CreatedAt time.Time             `json:"created_at"`
}
type TransitionChain struct {
	Cursor      CredentialFingerprint  `json:"cursor"`
	Transitions []CredentialTransition `json:"transitions,omitempty"`
}

func (c TransitionChain) Tail() CredentialFingerprint {
	if len(c.Transitions) > 0 {
		return c.Transitions[len(c.Transitions)-1].Next
	}
	return c.Cursor
}
func (c TransitionChain) Append(t CredentialTransition) TransitionChain {
	c.Transitions = append(c.Transitions, t)
	if len(c.Transitions) > 12 {
		c.Transitions = append([]CredentialTransition(nil), c.Transitions[len(c.Transitions)-12:]...)
		c.Cursor = c.Transitions[0].Prev
	}
	return c
}

type CredentialObservationKind string

const (
	CredentialSame          CredentialObservationKind = "same"
	CredentialOwnedRotation CredentialObservationKind = "owned_rotation"
	CredentialMetadataOnly  CredentialObservationKind = "metadata_only"
	CredentialExternalLogin CredentialObservationKind = "external_login"
	CredentialAmbiguous     CredentialObservationKind = "ambiguous"
)

type CredentialObservation struct {
	Kind    CredentialObservationKind
	Advance int
}

func ClassifyObservedCredential(chain TransitionChain, observed CredentialFingerprint) CredentialObservation {
	return ClassifyObservedCredentialAt(chain, observed, time.Now())
}
func ClassifyObservedCredentialAt(chain TransitionChain, observed CredentialFingerprint, now time.Time) CredentialObservation {
	if observed.CompositeHash == chain.Cursor.CompositeHash {
		return CredentialObservation{Kind: CredentialSame}
	}
	for i, tr := range chain.Transitions {
		if tr.Phase == TransitionApplied && observed.CompositeHash == tr.Next.CompositeHash {
			if tr.CreatedAt.IsZero() || now.Sub(tr.CreatedAt) <= 24*time.Hour {
				return CredentialObservation{Kind: CredentialOwnedRotation, Advance: i + 1}
			}
			return CredentialObservation{Kind: CredentialAmbiguous}
		}
	}
	for _, tr := range chain.Transitions {
		if tr.Phase == TransitionOutcomeUnknown {
			return CredentialObservation{Kind: CredentialAmbiguous}
		}
	}
	if observed.SubjectHash == chain.Cursor.SubjectHash && observed.RefreshTokenHash == chain.Cursor.RefreshTokenHash {
		return CredentialObservation{Kind: CredentialMetadataOnly}
	}
	return CredentialObservation{Kind: CredentialExternalLogin}
}
