package autonomy

import (
	"testing"
	"time"
)

func TestPolicyHashIsCanonical(t *testing.T) {
	a := DelegationPolicy{ObjectiveScope: "objective", AllowedTools: []string{"shell", "files"}, ConsequenceClasses: []ConsequenceClass{ExternalEffect, Observe}}
	b := DelegationPolicy{ObjectiveScope: "objective", AllowedTools: []string{"files", "shell"}, ConsequenceClasses: []ConsequenceClass{Observe, ExternalEffect}}
	_, ha, err := CanonicalPolicy(a)
	if err != nil {
		t.Fatal(err)
	}
	_, hb, err := CanonicalPolicy(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("hashes differ: %s %s", ha, hb)
	}
}

func TestPolicyRestrictionDelegationAndRevocation(t *testing.T) {
	parent := DelegationPolicy{AllowedTools: []string{"files", "shell"}, ConsequenceClasses: []ConsequenceClass{Observe, LocalMutation}, MaxConcurrency: 2, MaxDelegationDepth: 2, GeneratedCode: true}
	child := DelegationPolicy{AllowedTools: []string{"files"}, ConsequenceClasses: []ConsequenceClass{Observe}, MaxConcurrency: 1, MaxDelegationDepth: 1}
	if !IsRestriction(parent, child) {
		t.Fatal("valid restriction rejected")
	}
	if _, err := NarrowPolicy(parent, child); err != nil {
		t.Fatal(err)
	}
	expanded := child
	expanded.ConsequenceClasses = []ConsequenceClass{ExternalEffect}
	if _, err := NarrowPolicy(parent, expanded); err == nil {
		t.Fatal("authority expansion accepted")
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	if !parent.AllowsConsequence(Observe, now) {
		t.Fatal("allowed consequence denied")
	}
	parent.Revoked = true
	if parent.AllowsConsequence(Observe, now) {
		t.Fatal("revoked policy allowed action")
	}
}

func TestPolicyExpiration(t *testing.T) {
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Second)
	p := DelegationPolicy{ConsequenceClasses: []ConsequenceClass{Observe}, ExpiresAt: &expired}
	if p.AllowsConsequence(Observe, now) {
		t.Fatal("expired policy allowed action")
	}
}
