package runtime

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// /effort sets a session override that pins the per-turn thinking effort; "auto"
// clears it and returns to the turn_intent judge's baseline for the turn.
func TestEffortOverride(t *testing.T) {
	s := &Session{}

	s.SetEffortOverride("high")
	if !s.hasEffortOverride || s.EffortOverride() != "high" {
		t.Fatalf("override not applied: has=%v val=%q", s.hasEffortOverride, s.EffortOverride())
	}
	// An override pins the effort — the judge is skipped entirely.
	if s.shouldJudgeTurn() {
		t.Error("an /effort override must skip the turn_intent judge")
	}

	s.SetEffortOverride("auto")
	if s.hasEffortOverride {
		t.Error("\"auto\" must clear the override")
	}
}

// The reasoning tool's "auto" restores the turn's judged baseline — it never
// re-runs a classifier mid-turn.
func TestReasoningAutoRestoresJudgedBaseline(t *testing.T) {
	s := &Session{turnBaseEffort: wire.EffortMedium}
	s.setTurnEffort(wire.EffortHigh) // the tool bumped it mid-turn
	if s.turnBaseEffort != wire.EffortMedium {
		t.Fatalf("baseline must survive mid-turn adjustments, got %v", s.turnBaseEffort)
	}
}
