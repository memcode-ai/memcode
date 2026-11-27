package runtime

import (
	"io"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/wire"
)

// /effort sets the session's thinking effort on memcode's universal scale.
// There is no auto: the retired "auto" and anything unrecognized become medium,
// memcode's default, and a new session starts at medium.
func TestEffortOverride(t *testing.T) {
	if got := newSess(nil, &scriptProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard).EffortOverride(); got != "medium" {
		t.Fatalf("a new session must start at medium, got %q", got)
	}
	s := &Session{}
	for in, want := range map[string]string{
		"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max",
		"default": "default", "off": "off", " HIGH ": "high",
		"auto": "medium", "nonsense": "medium",
	} {
		s.SetEffortOverride(in)
		if got := s.EffortOverride(); got != want {
			t.Errorf("SetEffortOverride(%q) → %q, want %q", in, got, want)
		}
	}
}

// deepEffort gives the plan-sized output ceiling to efforts that can spend a
// large hidden-reasoning budget, the vendor default included.
func TestDeepEffort(t *testing.T) {
	for e, want := range map[wire.Effort]bool{
		wire.EffortOff: false, wire.EffortLow: false, wire.EffortMedium: false,
		wire.EffortHigh: true, wire.EffortXhigh: true, wire.EffortMax: true, wire.EffortDefault: true,
	} {
		if deepEffort(e) != want {
			t.Errorf("deepEffort(%q) = %v, want %v", e, !want, want)
		}
	}
}

// EffortLabel names the level a model actually runs: the chosen level, plus
// the native level when the model lacks the chosen one. Nothing for off or a
// non-reasoning model.
func TestEffortLabel(t *testing.T) {
	for _, c := range []struct {
		model string
		e     wire.Effort
		want  string
	}{
		{"opus", wire.EffortMedium, "medium effort"},
		{"opus", wire.EffortMax, "max effort"},
		{"grok-4.7", wire.EffortMax, "max effort, runs as xhigh"},
		{"gemini-pro", wire.EffortXhigh, "xhigh effort, runs as high"},
		{"gemini-4-carbon", wire.EffortDefault, "default effort"},
		{"opus", wire.EffortOff, ""},
		{"text-embedding-3-small", wire.EffortHigh, ""},
	} {
		if got := EffortLabel(c.model, c.e); got != c.want {
			t.Errorf("EffortLabel(%q, %q) = %q, want %q", c.model, c.e, got, c.want)
		}
	}
}

// The status line shows the last non-empty line of the streamed reasoning
// summary, with markdown emphasis trimmed; nothing once the call has ended.
func TestReasoningSnippet(t *testing.T) {
	s := &Session{}
	s.beginCall()
	s.onReasoning("**Reading the repo**\n\nChecking the ")
	s.onReasoning("moonlight directory\n")
	if got := s.ReasoningSnippet(); got != "Checking the moonlight directory" {
		t.Fatalf("snippet = %q", got)
	}
	s.onReasoning("\n**Planning the engine**\n")
	if got := s.ReasoningSnippet(); got != "Planning the engine" {
		t.Fatalf("snippet = %q", got)
	}
	// The line survives the end of the call and the tool runs that follow, so
	// it doesn't flash away between the calls of one turn...
	s.endCall()
	s.beginCall()
	if got := s.ReasoningSnippet(); got != "Planning the engine" {
		t.Fatalf("snippet between calls = %q, want the last summary kept", got)
	}
	// ...until the next call streams its own summary, which replaces it...
	s.onReasoning("Writing the tests")
	if got := s.ReasoningSnippet(); got != "Writing the tests" {
		t.Fatalf("snippet after the next call's reasoning = %q", got)
	}
	// ...and a new user turn starts clean.
	s.clearReasoning()
	if got := s.ReasoningSnippet(); got != "" {
		t.Fatalf("snippet at a new turn = %q, want empty", got)
	}
}

// A running sub-agent is listed live with its label, task and tool count, and
// drops off the list when it finishes.
func TestRunningSubagents(t *testing.T) {
	s := &Session{}
	sub := &Session{}
	s.trackSub(sub, subLabel(AgentSpec{Purpose: "explore", Scope: "moonlight"}), "Is Moonlight implemented?")
	sub.liveTools.Add(3)
	got := s.RunningSubagents()
	if len(got) != 1 || got[0].Label != "Explore(moonlight)" || got[0].Task != "Is Moonlight implemented?" || got[0].Tools != 3 {
		t.Fatalf("running = %+v", got)
	}
	s.untrackSub(sub)
	if got := s.RunningSubagents(); len(got) != 0 {
		t.Fatalf("finished sub-agent still listed: %+v", got)
	}
}
