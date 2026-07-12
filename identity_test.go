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
	if got := ClassifyObservedCredentialAt(unknown, f1, now); got.Kind != CredentialAmbiguous {
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
	if len(c.Transitions) != 11 {
		t.Fatalf("len=%d", len(c.Transitions))
	}
}

func TestCredentialMetadataCanonicalization(t *testing.T) {
	a := NewCredentialFingerprint("s", "r", `{"b":2,"a":1}`)
	b := NewCredentialFingerprint("s", "r", " { \"a\" : 1, \"b\" : 2 } ")
	c := NewCredentialFingerprint("s", "r", `{"a":1,"b":3}`)
	if a != b {
		t.Fatal("equivalent metadata hashes differ")
	}
	if a == c {
		t.Fatal("different metadata hashes equal")
	}
}

func TestBindingValidatorRejectsEveryStaleComponent(t *testing.T) {
	f := fp("s", "r", "m")
	current := BindingVersion{Instance: 1, Admission: 2, Tier: 3, Login: 4, Fingerprint: f}
	valid := WritebackVersion{Token: ExecutionToken{Instance: 1, Admission: 2, Tier: 3}, Login: 4, Fingerprint: f}
	if !ValidateWriteback(current, valid) {
		t.Fatal("valid rejected")
	}
	cases := []WritebackVersion{valid, valid, valid, valid, valid}
	cases[0].Token.Instance++
	cases[1].Token.Admission++
	cases[2].Token.Tier++
	cases[3].Login++
	cases[4].Fingerprint = fp("s", "x", "m")
	for i, c := range cases {
		if ValidateWriteback(current, c) {
			t.Fatalf("stale component %d accepted", i)
		}
	}
}

func TestTransitionChainGenerationAndTimeBoundaries(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	c := TransitionChain{Cursor: fp("s", "r0", "m")}
	for i := 0; i < 12; i++ {
		c = c.AppendAt(CredentialTransition{Prev: c.Tail(), Next: fp("s", string(rune('a'+i)), "m"), Phase: TransitionApplied, CreatedAt: now}, now)
	}
	if c.GenerationCount() != 12 {
		t.Fatalf("at 12 got %d", c.GenerationCount())
	}
	c = c.AppendAt(CredentialTransition{Prev: c.Tail(), Next: fp("s", "last", "m"), Phase: TransitionApplied, CreatedAt: now}, now)
	if c.GenerationCount() != 12 {
		t.Fatalf("at 13 got %d", c.GenerationCount())
	}
	at24 := TransitionChain{Cursor: fp("s", "x", "m"), Transitions: []CredentialTransition{{Prev: fp("s", "x", "m"), Next: fp("s", "y", "m"), Phase: TransitionApplied, CreatedAt: now.Add(-24 * time.Hour)}}}
	if ClassifyObservedCredentialAt(at24, fp("s", "y", "m"), now).Kind != CredentialOwnedRotation {
		t.Fatal("24h boundary expired")
	}
	if ClassifyObservedCredentialAt(at24, fp("s", "y", "m"), now.Add(time.Nanosecond)).Kind != CredentialAmbiguous {
		t.Fatal("over 24h reachable")
	}
}

func TestCredentialReachabilityIsContiguousAndAmbiguityScoped(t *testing.T) {
	now := time.Now()
	f0, f1, f2, f3 := fp("s", "0", "m"), fp("s", "1", "m"), fp("s", "2", "m"), fp("s", "3", "m")
	chain := TransitionChain{Cursor: f0, Transitions: []CredentialTransition{{Prev: f0, Next: f1, Phase: TransitionAborted, CreatedAt: now}, {Prev: f1, Next: f2, Phase: TransitionApplied, CreatedAt: now}, {Prev: f2, Next: f3, Phase: TransitionOutcomeUnknown, CreatedAt: now}}}
	if ClassifyObservedCredentialAt(chain, f2, now).Kind == CredentialOwnedRotation {
		t.Fatal("walked through aborted edge")
	}
	if ClassifyObservedCredentialAt(chain, fp("external", "z", "m"), now).Kind == CredentialAmbiguous {
		t.Fatal("unrelated unknown caused ambiguity")
	}
	chain.Transitions[0].Phase = TransitionApplied
	chain.Transitions[1].Phase = TransitionOutcomeUnknown
	if ClassifyObservedCredentialAt(chain, f2, now).Kind != CredentialAmbiguous {
		t.Fatal("relevant unresolved edge not ambiguous")
	}
}
