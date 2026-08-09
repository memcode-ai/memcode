package runtime

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"
)

func TestParseLessonReply(t *testing.T) {
	cases := []struct {
		in       string
		trigger  string
		strategy string
		ok       bool
	}{
		{"TRIGGER: editing Go imports by hand\nSTRATEGY: run diagnostics after the edit", "editing Go imports by hand", "run diagnostics after the edit", true},
		{"Some preamble\nTRIGGER: x breaks\nSTRATEGY: do y\ntrailing", "x breaks", "do y", true},
		{"TRIGGER: none", "", "", false},
		{"TRIGGER: none\nSTRATEGY: irrelevant", "", "", false},
		{"STRATEGY: only half the contract", "", "", false},
		{"no contract at all", "", "", false},
	}
	for _, c := range cases {
		tr, st, ok := parseLessonReply(c.in)
		if ok != c.ok || tr != c.trigger || st != c.strategy {
			t.Errorf("parseLessonReply(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, tr, st, ok, c.trigger, c.strategy, c.ok)
		}
	}
}

func TestEmitLessonSignalDualWrites(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	slog, err := sessionlog.Open(root, "sess-a")
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{root: root, store: st, out: io.Discard, turn: newTurnState(), sessionID: "sess-a", slog: slog}
	s.turn.editedPaths = map[string]bool{"a.go": true}

	s.emitLessonSignalFor(ctx, "trigger text here", "strategy text here", s.sessionID, "abc1234def", []string{"a.go"})
	_ = slog.Close()

	evs, err := st.ListEvents(ctx, store.EventFilter{Kinds: []string{string(events.KindLessonSignal)}, Limit: 10})
	if err != nil || len(evs) != 1 {
		t.Fatalf("expected 1 lesson_signal event, got %d (err %v)", len(evs), err)
	}
	var p struct {
		Trigger  string  `json:"trigger"`
		Strategy string  `json:"strategy"`
		Strength float64 `json:"strength"`
	}
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Trigger != "trigger text here" || p.Strategy != "strategy text here" || p.Strength != lessonStrength {
		t.Errorf("event payload wrong: %+v", p)
	}
	// Canonical copy in the session log.
	recs, err := sessionlog.LessonSignals(root)
	if err != nil || len(recs) != 1 {
		t.Fatalf("expected 1 canonical lesson record, got %d (err %v)", len(recs), err)
	}
	if recs[0].Trigger != "trigger text here" || recs[0].SessionID != "sess-a" {
		t.Errorf("canonical record wrong: %+v", recs[0])
	}
}

// TestBackfillLessonSignals: wipe the derived index → signals recover from the
// canonical events.jsonl (the persistence doctrine, applied to lessons).
func TestBackfillLessonSignals(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	slog, err := sessionlog.Open(root, "sess-b")
	if err != nil {
		t.Fatal(err)
	}
	slog.Append(sessionlog.Record{Kind: sessionlog.KindLessonSignal,
		Trigger: "recovered trigger", Strategy: "recovered strategy", Strength: 0.6})
	_ = slog.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db")) // fresh db = wiped index
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Session{root: root, store: st, out: io.Discard, turn: newTurnState()}
	s.backfillLessonSignals(ctx)

	evs, err := st.ListEvents(ctx, store.EventFilter{Kinds: []string{string(events.KindLessonSignal)}, Limit: 10})
	if err != nil || len(evs) != 1 {
		t.Fatalf("expected 1 recovered event, got %d (err %v)", len(evs), err)
	}
	if !strings.Contains(string(evs[0].Payload), "recovered trigger") {
		t.Errorf("recovered payload wrong: %s", evs[0].Payload)
	}
	// Idempotent: running again must not double-count.
	s.backfillLessonSignals(ctx)
	evs, _ = st.ListEvents(ctx, store.EventFilter{Kinds: []string{string(events.KindLessonSignal)}, Limit: 10})
	if len(evs) != 1 {
		t.Errorf("backfill must be idempotent, got %d events", len(evs))
	}
}

// TestDistillLessonNoRunner: a bare session (no gateway) must not panic or spin.
func TestDistillLessonNoRunner(t *testing.T) {
	s := &Session{out: io.Discard, turn: newTurnState()}
	s.turn.firstBreak = "some failure"
	s.distillLesson("fixed it") // runner nil → returns immediately
}
