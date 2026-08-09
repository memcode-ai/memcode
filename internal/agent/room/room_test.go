package room

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/mood"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/store"
)

func TestAssessModesAndPolicy(t *testing.T) {
	cases := []struct {
		name       string
		reading    mood.Reading
		sig        Signals
		wantMode   Mode
		wantWrite  bool // expected Policy.AllowAutoWrite
		wantIntent Intent
	}{
		{"angry → repair", mood.Reading{State: mood.Angry, Frustration: 0.9}, Signals{}, Repair, false, Correcting},
		{"frustrated → repair", mood.Reading{State: mood.Frustrated, Frustration: 0.6}, Signals{}, Repair, false, Correcting},
		{"curious → explore", mood.Reading{State: mood.Curious}, Signals{}, Explore, false, Exploring},
		{"confused → explain", mood.Reading{State: mood.Confused}, Signals{}, Explain, false, Understanding},
		{"urgent → execute", mood.Reading{State: mood.Urgent}, Signals{}, Execute, true, Executing},
		{"calm → normal", mood.Reading{State: mood.Calm}, Signals{}, Normal, true, Working},
		{"focused → normal", mood.Reading{State: mood.Focused}, Signals{}, Normal, true, Working},
		{"loop stuck → replan", mood.Reading{State: mood.Focused}, Signals{LoopRisk: Stuck}, Replan, false, Working},
		{"denials → repair via low trust", mood.Reading{State: mood.Focused}, Signals{Denials: 2}, Repair, false, Working},
		{"rejected outcome → repair", mood.Reading{State: mood.Focused}, Signals{LastOutcome: OutcomeRejected}, Repair, false, Working},
	}
	for _, c := range cases {
		s := Assess(c.reading, c.sig)
		if s.Mode != c.wantMode {
			t.Errorf("%s: mode = %q, want %q", c.name, s.Mode, c.wantMode)
		}
		if s.Policy.AllowAutoWrite != c.wantWrite {
			t.Errorf("%s: AllowAutoWrite = %v, want %v", c.name, s.Policy.AllowAutoWrite, c.wantWrite)
		}
		if s.Intent != c.wantIntent {
			t.Errorf("%s: intent = %q, want %q", c.name, s.Intent, c.wantIntent)
		}
	}
}

func TestRepairCarriesStrongMemoryWeight(t *testing.T) {
	s := Assess(mood.Reading{State: mood.Angry, Frustration: 0.9}, Signals{})
	if s.Policy.MemoryWeight != "strong" {
		t.Fatalf("repair memory weight = %q, want strong", s.Policy.MemoryWeight)
	}
	if s.Mode != Repair {
		t.Fatalf("high frustration should select Repair mode, got %v", s.Mode)
	}
	// NB: the room's prose (Guidance) is server-owned now; the CLI only selects the Mode.
}

func TestUrgentIsTerse(t *testing.T) {
	s := Assess(mood.Reading{State: mood.Urgent}, Signals{})
	if !s.Policy.Terse {
		t.Fatal("execute mode should be terse")
	}
}

func TestGather(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const sid = "sess-current"
	emit := func(session string, kind events.Kind, payload map[string]any) {
		if payload == nil {
			payload = map[string]any{}
		}
		payload["session_id"] = session
		if _, err := events.Append(ctx, st, kind, "test", payload); err != nil {
			t.Fatal(err)
		}
	}
	emit(sid, events.KindInputInterrupted, nil)
	emit(sid, events.KindActionDenied, map[string]any{"action": "edit x"})
	// Same command 4× → stuck loop.
	for i := 0; i < 4; i++ {
		emit(sid, events.KindCommandExecuted, map[string]any{"command": "go test ./...", "exit": 1})
	}
	emit(sid, events.KindTestRun, map[string]any{"command": "go test", "exit": 1})
	emit(sid, events.KindSessionOutcome, map[string]any{"outcome": "rejected"})

	sig := Gather(ctx, st, sid)
	if sig.Interrupts != 1 {
		t.Errorf("interrupts = %d, want 1", sig.Interrupts)
	}
	if sig.Denials != 1 {
		t.Errorf("denials = %d, want 1", sig.Denials)
	}
	if sig.LoopRisk != Stuck {
		t.Errorf("loop risk = %q, want stuck", sig.LoopRisk)
	}
	if sig.LastOutcome != OutcomeRejected {
		t.Errorf("outcome = %q, want rejected", sig.LastOutcome)
	}

	// A DIFFERENT (fresh, calm) session must NOT inherit the above friction/loop — the
	// room is about now, this session only.
	fresh := Gather(ctx, st, "sess-fresh")
	if fresh.LoopRisk != NoLoop || fresh.Interrupts != 0 || fresh.Denials != 0 || fresh.LastOutcome != OutcomeUnknown {
		t.Errorf("a fresh session must not inherit prior-session friction, got %+v", fresh)
	}
}
