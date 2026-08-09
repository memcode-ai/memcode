package runtime

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

func reasonCall(t *testing.T, s *Session, in string) toolResult {
	t.Helper()
	return s.reasoningTool(context.Background(), json.RawMessage(in))
}

// Self-adjust: the model can raise/drop its OWN effort mid-turn; the change
// lands in turnEffort (read by runLoop on every iteration) and a bare call
// reports the current depth.
func TestReasoningSelfAdjust(t *testing.T) {
	s := &Session{out: io.Discard}
	r := reasonCall(t, s, `{"effort":"high"}`)
	if r.isError || s.ThinkingEffort() != "high" {
		t.Fatalf("set high: %q err=%v effort=%q", r.text(), r.isError, s.ThinkingEffort())
	}
	r = reasonCall(t, s, `{}`)
	if r.isError || !strings.Contains(r.text(), "high") {
		t.Fatalf("bare call must report the current depth: %q", r.text())
	}
	r = reasonCall(t, s, `{"effort":"off"}`)
	if r.isError || s.ThinkingEffort() != "" {
		t.Fatalf("set off: %q effort=%q", r.text(), s.ThinkingEffort())
	}
	if r = reasonCall(t, s, `{"effort":"sideways"}`); !r.isError {
		t.Fatal("junk level must error")
	}
}

// /effort is the session DEFAULT, not a cage: the model may override it
// mid-turn (the default returns at the next turn's scoring), and `auto` means
// "back to the default" — the user's setting when one exists.
func TestReasoningOverridesSessionDefaultMidTurn(t *testing.T) {
	s := &Session{out: io.Discard, effortOverride: wire.EffortMedium, hasEffortOverride: true}
	s.setTurnEffort(wire.EffortMedium)
	r := reasonCall(t, s, `{"effort":"high"}`)
	if r.isError || s.ThinkingEffort() != "high" {
		t.Fatalf("the model must be able to override the default mid-turn: %q effort=%q", r.text(), s.ThinkingEffort())
	}
	if !strings.Contains(r.text(), "medium") {
		t.Fatalf("result should name the default that returns at turn end: %q", r.text())
	}
	if r = reasonCall(t, s, `{}`); !strings.Contains(r.text(), "session default: medium") {
		t.Fatalf("report must show current AND the session default: %q", r.text())
	}
	// auto = hand back to the default the user chose.
	if r = reasonCall(t, s, `{"effort":"auto"}`); r.isError || s.ThinkingEffort() != "medium" {
		t.Fatalf("auto must restore the session default: %q effort=%q", r.text(), s.ThinkingEffort())
	}
}

// Delegation validates its inputs before spawning anything.
func TestReasoningDelegateValidation(t *testing.T) {
	s := &Session{out: io.Discard}
	if r := reasonCall(t, s, `{"task":"why does X deadlock","effort":"off"}`); !r.isError {
		t.Fatal("a delegate with thinking off is a contradiction — must error")
	}
	if r := reasonCall(t, s, `{"task":"why does X deadlock","effort":"auto"}`); !r.isError {
		t.Fatal("auto is a self-adjust level, not a delegate depth — must error")
	}
}
