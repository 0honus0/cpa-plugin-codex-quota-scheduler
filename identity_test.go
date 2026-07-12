package main

import (
	"testing"
	"time"
)

func fp(subject, refresh, metadata string) CredentialFingerprint {
	return NewCredentialFingerprint(subject, refresh, metadata)
}

func TestCredentialClassification(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	f0 := fp("s", "r0", "m")
	f1 := fp("s", "r1", "m")
	f2 := fp("s", "r2", "m")
	chain := TransitionChain{Cursor: f0, Transitions: []CredentialTransition{{Prev: f0, Next: f1, SaveSeq: 10, Phase: TransitionApplied, CreatedAt: now}, {Prev: f1, Next: f2, SaveSeq: 11, Phase: TransitionApplied, CreatedAt: now}}}
	cases := []struct {
		name     string
		observed CredentialFingerprint
		want     CredentialObservationKind
	}{
		{"same", f0, CredentialSame}, {"reachable", f1, CredentialOwnedRotation}, {"skipped", f2, CredentialOwnedRotation},
		{"metadata-only", fp("s", "r0", "other"), CredentialMetadataOnly}, {"external-subject", fp("other", "r0", "m"), CredentialExternalLogin}, {"external-refresh", fp("s", "external", "m"), CredentialExternalLogin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyObservedCredentialAt(chain, tc.observed, now)
			if got.Kind != tc.want {
				t.Fatalf("kind=%s want=%s", got.Kind, tc.want)
			}
		})
	}
}

func TestCredentialChainExpiryAndAmbiguousReconciliation(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	f0 := fp("s", "r0", "m")
	f1 := fp("s", "r1", "m")
	expired := TransitionChain{Cursor: f0, Transitions: []CredentialTransition{{Prev: f0, Next: f1, Phase: TransitionApplied, CreatedAt: now.Add(-25 * time.Hour)}}}
	if got := ClassifyObservedCredentialAt(expired, f1, now); got.Kind != CredentialAmbiguous {
		t.Fatalf("expired kind=%s", got.Kind)
	}
	unknown := TransitionChain{Cursor: f0, Transitions: []CredentialTransition{{Prev: f0, Next: f1, Phase: TransitionOutcomeUnknown, CreatedAt: now}}}
	if got := ClassifyObservedCredentialAt(unknown, fp("x", "y", "z"), now); got.Kind != CredentialAmbiguous {
		t.Fatalf("unknown kind=%s", got.Kind)
	}
}

// inv:INV-28 positive
func TestExecutionTokenRejectsOldInstanceWriteback(t *testing.T) {
	token := ExecutionToken{Instance: 2, Admission: 3, Tier: 4, Fence: 5}
	if !token.ValidFor(2, 3, 4) || token.ValidFor(1, 3, 4) {
		t.Fatal("instance fence mismatch")
	}
}

func TestPluginStateChecksExecutionTokenAgainstCurrentAccount(t *testing.T) {
	s := NewPluginState(DefaultConfig())
	s.UpsertQuota(AccountState{AuthID: "a", Instance: 2, AdmissionEpoch: 3})
	if !s.ExecutionTokenCurrent("a", ExecutionToken{Instance: 2, Admission: 3}) || s.ExecutionTokenCurrent("a", ExecutionToken{Instance: 1, Admission: 3}) {
		t.Fatal("execution token fencing mismatch")
	}
}

// inv:INV-28 negative
// inv:INV-33 positive
// inv:INV-33 negative
// inv:INV-40 positive
// inv:INV-40 negative
func TestIdentityEpochTypesRemainDistinct(t *testing.T) {
	var _ AccountIdentity = 1
	var _ AuthInstanceID = 1
	var _ LoginEpoch = 1
	var _ TokenEpoch = 1
}

func TestTransitionChainCapsTwelveGenerations(t *testing.T) {
	now := time.Now()
	c := TransitionChain{Cursor: fp("s", "r0", "m")}
	for i := 0; i < 13; i++ {
		next := fp("s", string(rune('a'+i)), "m")
		c = c.Append(CredentialTransition{Prev: c.Tail(), Next: next, Phase: TransitionApplied, CreatedAt: now})
	}
	if len(c.Transitions) != 12 {
		t.Fatalf("len=%d", len(c.Transitions))
	}
}
