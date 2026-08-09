package runtime

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/room"
	"github.com/memcode-ai/memcode/internal/wire"
)

// TestMainLoopDoesNotHardcodeThinking guards the regression: the executive loop
// must NOT force EffortMedium on every call.
func TestMainLoopDoesNotHardcodeThinking(t *testing.T) {
	// (Behavioral coverage lives in turnintent_test.go; this asserts the field path
	// exists so the loop reads per-turn effort rather than a constant.)
	s := &Session{turnEffort: wire.EffortOff}
	if s.turnEffort != wire.EffortOff {
		t.Fatal("turnEffort default should be off")
	}
}

func TestRoutingHintForTurn(t *testing.T) {
	// An ordinary calm turn → no hint (default routing; the 80% stays on the cheap lane).
	if h := routingHintForTurn(room.State{}, false, false); h != nil {
		t.Fatalf("a calm turn must produce no escalation hint, got %+v", h)
	}
	// Mid-repair (the agent's own edit broke a file) → the failure valve.
	if h := routingHintForTurn(room.State{}, true, false); h == nil || h.Reason != "self_heal" {
		t.Fatalf("self-heal must escalate, got %+v", h)
	}
	// Active room friction → an escalation hint whose reason names the SPECIFIC
	// signal (an "elevated" room must not claim "high").
	for _, c := range []struct {
		rm   room.State
		want string
	}{
		{room.State{Friction: "high"}, "user_friction_high"},
		{room.State{Friction: "elevated"}, "user_friction_elevated"},
		{room.State{Mode: room.Repair}, "room_repair"},
		{room.State{Mode: room.Replan}, "room_replan"},
		{room.State{Intent: room.Correcting}, "user_correcting"},
	} {
		h := routingHintForTurn(c.rm, false, false)
		if h == nil {
			t.Fatalf("friction %+v must produce an escalate hint, got nil", c.rm)
		}
		if h.Reason != c.want {
			t.Errorf("friction %+v reason = %q, want %q", c.rm, h.Reason, c.want)
		}
	}
	// High-risk surface → an escalate hint with the risk reason.
	if h := routingHintForTurn(room.State{}, false, true); h == nil || h.Reason != "high_risk_surface" {
		t.Fatalf("a high-risk surface must escalate, got %+v", h)
	}
	// Self-heal takes precedence over a friction-only hint.
	if h := routingHintForTurn(room.State{Friction: "high"}, true, false); h == nil || h.Reason != "self_heal" {
		t.Fatalf("a failed attempt must win as the valve, got %+v", h)
	}
}

func TestHighRiskTurn(t *testing.T) {
	// Destructive ops escalate when they appear as an ACTUAL command (a `$`/backtick span the
	// shell classifier rates catastrophic) — not from a substring in prose.
	for _, q := range []string{
		"clean the build: `rm -rf ./build`", "$ git push --force origin main",
		"run `git reset --hard HEAD~1` to undo", "wipe it with\n```\nrm -rf /tmp/x\n```",
	} {
		if !highRiskTurn(q) {
			t.Errorf("destructive command %q must be high-risk", q)
		}
	}
	// Prose that merely NAMES destructive ops (the real bug: a plan listing ops to GUARD) must
	// NOT be high-risk — the shell parser never sees a command, only a mention.
	for _, q := range []string{
		"still stops for dangerous ops (rm -rf, force-push, secrets)",
		"the yolo build should never silently run rm -rf or force push",
		"explain what reset --hard does",
	} {
		if highRiskTurn(q) {
			t.Errorf("prose mention %q must NOT be high-risk (talking-about != doing)", q)
		}
	}
	// Security/billing surface WITH edit intent escalates.
	for _, q := range []string{
		"add the stripe billing webhook", "change the auth middleware",
		"rotate the api secret", "write the jwt login flow", "create the schema migration",
	} {
		if !highRiskTurn(q) {
			t.Errorf("risky edit %q must be high-risk", q)
		}
	}
	// Bare lookups over the same surfaces are NOT high-risk (don't sink the 80/20).
	for _, q := range []string{
		"what does the auth token do?", "where is the billing code?",
		"explain the migration", "how does the login session work",
	} {
		if highRiskTurn(q) {
			t.Errorf("read-only %q must NOT be high-risk (no edit intent)", q)
		}
	}
}

// NOTE: the per-turn MODEL tier decision moved server-side — its coverage lives in
// api/internal/provider/resolve_test.go (TestResolveModel). Thinking + tier are judged
// by the turn_intent classifier (turnintent_test.go); the CLI tests here keep only the
// deterministic room/risk FACTS it still owns.

// TestReasoningTier locks the honest, lane-aware status label: GLM (and other hybrid open models)
// reason at high normally / max on hard turns; Anthropic adaptive models show their effort tier;
// non-reasoning models show nothing. No more misleading "effort: off" on the cheap lane.
func TestReasoningTier(t *testing.T) {
	cases := []struct {
		model string
		eff   wire.Effort
		want  string
	}{
		{"glm-5p2", wire.EffortOff, "high"},         // ordinary GLM turn → high (NOT off)
		{"glm-5p2", wire.EffortHigh, "max"},         // hard GLM turn → max
		{"qwen-3.7-max", wire.EffortMedium, "high"}, // any hybrid model, ordinary → high
		{"claude-opus-5", wire.EffortHigh, "high"},  // Anthropic adaptive → verbatim tier
		{"claude-sonnet-5", wire.EffortOff, ""},     // Anthropic, off → no extended thinking
		{"some-llama-3", wire.EffortHigh, ""},       // non-reasoning model → nothing
	}
	for _, c := range cases {
		if got := reasoningTier(c.model, c.eff); got != c.want {
			t.Errorf("reasoningTier(%q,%v)=%q want %q", c.model, c.eff, got, c.want)
		}
	}
}
