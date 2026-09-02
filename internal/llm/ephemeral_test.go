package llm

import (
	"context"
	"testing"
)

// The ephemeral review override is the ONE remaining way a model other than the
// session's pin can serve a turn, which makes it the seam through which
// per-request model switching could grow back. These tests are load-bearing,
// not ceremony.

// ForkWithModel serves the named model and leaves the parent's pin alone.
func TestForkWithModelIsEphemeral(t *testing.T) {
	p := &scriptedProv{}
	parent := pinnedRunner(p, prodInfo(nil), "sonnet")

	review := parent.ForkWithModel("opus")
	if _, err := review.Complete(context.Background(), Review, userReq("critique this")); err != nil {
		t.Fatal(err)
	}
	if p.requested[0] != "opus" {
		t.Fatalf("the review ran on %q, want the named model (opus)", p.requested[0])
	}

	// The parent is untouched: its next turn is still the session's model.
	if _, err := parent.Complete(context.Background(), MainLoop, userReq("keep going")); err != nil {
		t.Fatal(err)
	}
	if p.requested[1] != "sonnet" {
		t.Fatalf("after a review the session ran on %q, want the pin (sonnet)", p.requested[1])
	}
	if parent.pin != "sonnet" {
		t.Fatalf("the parent's pin is now %q — an ephemeral override must not persist", parent.pin)
	}
}

// A fork that is NOT given an override inherits the pin, so ordinary sub-agents
// keep running on the user's model.
func TestPlainForkStillInheritsThePin(t *testing.T) {
	p := &scriptedProv{}
	parent := pinnedRunner(p, prodInfo(nil), "opus")
	if _, err := parent.Fork().Complete(context.Background(), Agent, userReq("delegated work")); err != nil {
		t.Fatal(err)
	}
	if p.requested[0] != "opus" {
		t.Fatalf("a delegated worker ran on %q, want the session pin (opus)", p.requested[0])
	}
}

// Nothing runs a second model unless it was asked for: an ordinary turn issues
// exactly one call, on the pin.
func TestNoSecondModelWithoutAnExplicitReview(t *testing.T) {
	p := &scriptedProv{}
	r := pinnedRunner(p, prodInfo(nil), "sonnet")
	if _, err := r.Complete(context.Background(), MainLoop, userReq("do it")); err != nil {
		t.Fatal(err)
	}
	if len(p.requested) != 1 {
		t.Fatalf("%d calls for one turn (%v) — a turn must not summon a second model on its own", len(p.requested), p.requested)
	}
}
