package runtime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/store"
)

func TestParseAdherenceReply(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`{"verdicts":[{"id":"lesson_ab","verdict":"followed"},{"id":"prefcand_cd","verdict":"violated"}]}`, 2},
		{"```json\n{\"verdicts\":[{\"id\":\"lesson_ab\",\"verdict\":\"not_applicable\"}]}\n```", 1},
		{"Sure! Here you go: {\"verdicts\":[{\"id\":\"x\",\"verdict\":\"followed\"}]}", 1},
		{"no json at all", 0},
		{`{"verdicts":[]}`, 0},
	}
	for _, c := range cases {
		if got := parseAdherenceReply(c.in); len(got) != c.want {
			t.Errorf("parseAdherenceReply(%q) = %d verdicts, want %d", c.in, len(got), c.want)
		}
	}
}

func outcomesSession(t *testing.T) (*Session, store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Session{root: t.TempDir(), store: st, out: io.Discard, turn: newTurnState(), sessionID: "current"}, st
}

func appendEvt(t *testing.T, st store.Store, kind events.Kind, payload map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	if _, err := st.AppendEvent(context.Background(), store.Event{Kind: string(kind), Payload: raw}); err != nil {
		t.Fatal(err)
	}
}

// unprocessedOutcomes: assembles outcome+baseline+files+rules per session,
// skips processed ones and the current session, caps the batch.
func TestUnprocessedOutcomes(t *testing.T) {
	s, st := outcomesSession(t)
	ctx := context.Background()

	// Session A: corrected, has baseline, files, and inlined rules — a full target.
	appendEvt(t, st, events.KindAgentSessionStarted, map[string]any{"session_id": "a", "head_sha": "aaa111"})
	appendEvt(t, st, events.KindFileEdited, map[string]any{"session_id": "a", "path": "x.go", "hash": "h1"})
	appendEvt(t, st, events.KindFileEdited, map[string]any{"session_id": "a", "path": "x.go", "hash": "h2"}) // dedup
	appendEvt(t, st, events.KindContextInlined, map[string]any{"session_id": "a", "lesson_ids": []string{"lesson_1"}, "pref_ids": []string{"prefcand_2"}})
	appendEvt(t, st, events.KindSessionOutcome, map[string]any{"session_id": "a", "outcome": "corrected", "evidence": "1/1 changed"})

	// Session B: already processed. The marker names its target in
	// target_session (emit stamps session_id with the judging session — a
	// marker keyed on session_id would mark the WRONG session; regression
	// from the live E2E).
	appendEvt(t, st, events.KindSessionOutcome, map[string]any{"session_id": "b", "outcome": "accepted"})
	appendEvt(t, st, events.KindOutcomeProcessed, map[string]any{"target_session": "b", "session_id": "some-other-judging-session"})

	// The CURRENT session must never be a target.
	appendEvt(t, st, events.KindSessionOutcome, map[string]any{"session_id": "current", "outcome": "accepted"})

	got := s.unprocessedOutcomes(ctx)
	if len(got) != 1 {
		t.Fatalf("want exactly session a, got %+v", got)
	}
	a := got[0]
	if a.id != "a" || a.outcome != "corrected" || a.baseline != "aaa111" {
		t.Errorf("session a assembled wrong: %+v", a)
	}
	if len(a.files) != 1 || a.files[0] != "x.go" {
		t.Errorf("files should dedup to [x.go]: %v", a.files)
	}
	if len(a.lessonIDs) != 1 || len(a.prefIDs) != 1 {
		t.Errorf("inlined rules lost: %+v", a)
	}
}

// ruleLines resolves ids to current rule text and silently drops rules deleted
// since the session ran.
func TestRuleLines(t *testing.T) {
	s, _ := outcomesSession(t)
	prefDir := filepath.Join(s.root, ".memcode", "prefs")
	if err := os.MkdirAll(prefDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pref := "# stage only the exact files you edited\n\n- **axis:** workflow\n- **weight:** 2.40\n"
	if err := os.WriteFile(filepath.Join(prefDir, "prefcand_aa11-stage-only.md"), []byte(pref), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := s.ruleLines(outcomeSession{
		prefIDs:   []string{"prefcand_aa11", "prefcand_gone"},
		lessonIDs: []string{"lesson_gone"},
	})
	if len(rules) != 1 {
		t.Fatalf("want 1 resolvable rule, got %v", rules)
	}
	if rules["prefcand_aa11"] == "" {
		t.Errorf("pref text should resolve: %v", rules)
	}
}

func TestParseFactsReply(t *testing.T) {
	reply := "```json\n[{\"fact\":\"Tim adopted a retriever named Biscuit.\",\"entities\":[\"Tim\",\" biscuit \"]},{\"fact\":\"\"},{\"fact\":\"The deploy runbook moved to infra/.\",\"entities\":[\"runbook\"]}]\n```"
	facts := parseFactsReply(reply)
	if len(facts) != 2 {
		t.Fatalf("want 2 facts (empty one dropped), got %d", len(facts))
	}
	if facts[0].Entities[0] != "tim" || facts[0].Entities[1] != "biscuit" {
		t.Fatalf("entities not normalized: %+v", facts[0].Entities)
	}
	if parseFactsReply("no json here") != nil {
		t.Fatal("malformed reply must drop silently")
	}
}
