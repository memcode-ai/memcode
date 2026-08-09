package vxui

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
)

// approvalEnterAction must never interrupt the turn on a bare Enter. The bug: with the "tell"
// option highlighted and nothing typed, Enter sent Interrupt and silently stopped the whole turn.
// Now an empty "tell" Enter is a no-op hint; only typed feedback is a "tell" (deny + redirect).
// "cancel" (scoped cards, e.g. MCP) is the opposite: a bare Enter IS the deny, no hint dance.
func TestApprovalEnterAction(t *testing.T) {
	cases := []struct {
		kind        string
		hasFeedback bool
		want        string
	}{
		{"yes", false, "yes"},           // Enter on Yes/Execute → allow
		{"remember", false, "remember"}, // Enter on don't-ask-again → allow + remember
		{"scope", false, "scope"},       // Enter on a scoped remember → allow + that scope
		{"cancel", false, "cancel"},     // Enter on Cancel → plain deny, turn continues
		{"tell", false, "hint"},         // Enter on "tell" with NOTHING typed → hint, NOT interrupt (the fix)
		{"tell", true, "tell"},          // "tell" with typed feedback → deny + redirect
		{"yes", true, "tell"},           // anything typed is a redirect regardless of the highlighted option
	}
	for _, c := range cases {
		if got := approvalEnterAction(c.kind, c.hasFeedback); got != c.want {
			t.Errorf("approvalEnterAction(%q, %v) = %q, want %q", c.kind, c.hasFeedback, got, c.want)
		}
	}
}

// A skill-load card's decline is a plain Skip (deny, turn continues), NOT a "tell" — Enter on it
// must act immediately instead of stalling on the type-feedback hint. The bug: option 3 was
// "No, and tell me what to do differently"; a bare Enter on it only reprinted the hint, so the
// card looked stuck and declining a skill needlessly interrupted the whole turn.
func TestSkillApprovalDeclineIsSkip(t *testing.T) {
	s := &appState{pending: &runtime.ApprovalRequest{Label: "Use skill", Title: "imessage:configure"}}
	opts := s.approvalOptions()
	if len(opts) != 3 {
		t.Fatalf("skill card has %d options, want 3", len(opts))
	}
	last := opts[2]
	if last.kind != "cancel" {
		t.Errorf("skill card decline kind = %q, want %q (plain deny, no interrupt)", last.kind, "cancel")
	}
	if last.label != "Skip (esc)" {
		t.Errorf("skill card decline label = %q, want %q", last.label, "Skip (esc)")
	}
	// A bare Enter on Skip must BE the deny — no hint dance.
	if got := approvalEnterAction(last.kind, false); got != "cancel" {
		t.Errorf("Enter on Skip = %q, want %q", got, "cancel")
	}
}
