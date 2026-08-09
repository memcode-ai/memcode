package recall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/store"
)

func seedStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// A current claim about the package manager.
	if err := st.AddClaim(ctx, store.Claim{
		ID: "c-pnpm", Type: "command", Text: "Use pnpm as the package manager, never npm",
		Scope: ".", Status: "current", Confidence: "high", ExtractedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// A current claim about TypeScript.
	if err := st.AddClaim(ctx, store.Claim{
		ID: "c-ts", Type: "preference", Text: "Write plain JavaScript; no TypeScript in this project",
		Scope: ".", Status: "current", Confidence: "high", ExtractedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// A STALE claim mentioning npm — should normally rank low, but surface when the
	// query is about stale/old npm usage.
	if err := st.AddClaim(ctx, store.Claim{
		ID: "c-oldnpm", Type: "command", Text: "Old setup used npm install for dependencies",
		Scope: ".", Status: "stale", Confidence: "low", ExtractedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// A human decision.
	if _, err := st.AppendEvent(ctx, store.Event{
		Kind: "decision", Actor: "user",
		Payload: []byte(`{"text":"We decided to drop TypeScript and write plain JavaScript"}`),
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func topText(t *testing.T, st store.Store, query string) string {
	t.Helper()
	hits, err := Recall(context.Background(), st, t.TempDir(), query, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatalf("query %q returned no hits", query)
	}
	return hits[0].Chunk.Text
}

func TestRecallTypeScript(t *testing.T) {
	st := seedStore(t)
	got := topText(t, st, "are we allowed to use typescript")
	if !strings.Contains(strings.ToLower(got), "typescript") {
		t.Fatalf("top hit for typescript = %q", got)
	}
}

func TestRecallPnpm(t *testing.T) {
	st := seedStore(t)
	got := topText(t, st, "which package manager pnpm")
	if !strings.Contains(got, "pnpm") {
		t.Fatalf("top hit for pnpm = %q", got)
	}
}

func TestStaleDownweightedUnlessAsked(t *testing.T) {
	st := seedStore(t)

	// A plain "npm" query: the stale npm claim must NOT outrank the current pnpm
	// claim (which also names npm) — current beats stale.
	hits, err := Recall(context.Background(), st, t.TempDir(), "npm", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Chunk.ID != "c-pnpm" {
		t.Fatalf("plain npm query top hit = %+v, want the current pnpm claim", firstID(hits))
	}

	// An explicit history query ("old npm") should surface the stale claim.
	hits, err = Recall(context.Background(), st, t.TempDir(), "old npm setup", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !containsID(hits, "c-oldnpm") {
		t.Fatalf("history query did not surface the stale npm claim: %v", allIDs(hits))
	}
}

func TestRecallEmptyCorpus(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hits, err := Recall(ctx, st, t.TempDir(), "anything", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("empty corpus should yield no hits, got %d", len(hits))
	}
}

func firstID(hits []Hit) string {
	if len(hits) == 0 {
		return "(none)"
	}
	return hits[0].Chunk.ID
}

func containsID(hits []Hit, id string) bool {
	for _, h := range hits {
		if h.Chunk.ID == id {
			return true
		}
	}
	return false
}

func allIDs(hits []Hit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.Chunk.ID
	}
	return ids
}

// Promoted rules (confirmed prefs + distilled lessons) are recallable — the
// long-term memory the user taught the agent, not just inlined context.
func TestRecallSurfacesPromotedRules(t *testing.T) {
	st := seedStore(t)
	root := t.TempDir()
	prefDir := filepath.Join(root, ".memcode", "prefs")
	if err := os.MkdirAll(prefDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pref := "# Never use em-dashes in prose written for the user\n\n- **axis:** style\n- **weight:** 2.40\n"
	if err := os.WriteFile(filepath.Join(prefDir, "prefcand_ab12-never-em-dashes.md"), []byte(pref), 0o644); err != nil {
		t.Fatal(err)
	}
	lessonDir := filepath.Join(root, ".memcode", "lessons")
	if err := os.MkdirAll(lessonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lesson := "# Lesson: vendored zsh globbing breaks on bare equals tokens\n\n- **strategy:** quote equals-prefixed args in shell commands\n- **weight:** 2.10\n"
	if err := os.WriteFile(filepath.Join(lessonDir, "lesson_cd34-zsh-globbing.md"), []byte(lesson), 0o644); err != nil {
		t.Fatal(err)
	}

	hits, err := Recall(context.Background(), st, root, "what is my preference about em-dashes in prose", "", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("recall over rules failed (err %v, %d hits)", err, len(hits))
	}
	if hits[0].Chunk.Cat != CatRule || !strings.Contains(hits[0].Chunk.Text, "em-dashes") {
		t.Errorf("top hit should be the promoted pref, got %s %q", hits[0].Chunk.Cat, hits[0].Chunk.Source)
	}

	hits, err = Recall(context.Background(), st, root, "zsh globbing equals tokens lesson", "", 5)
	if err != nil || len(hits) == 0 || hits[0].Chunk.Kind != "lesson" {
		t.Fatalf("lesson should be recallable, got %+v (err %v)", hits, err)
	}
}

// session_outcome events are recallable as prose ("what happened to my last change").
func TestRecallSessionOutcomes(t *testing.T) {
	st := seedStore(t)
	if _, err := st.AppendEvent(context.Background(), store.Event{
		Kind: "session_outcome", Actor: "reconciler",
		Payload: []byte(`{"session_id":"sess-42","outcome":"corrected","evidence":"2/3 agent file(s) manually changed after the agent"}`),
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := Recall(context.Background(), st, t.TempDir(), "corrected agent session outcome", "", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("outcome recall failed (err %v)", err)
	}
	if !strings.Contains(hits[0].Chunk.Text, "sess-42") || !strings.Contains(hits[0].Chunk.Text, "corrected") {
		t.Errorf("top hit should be the outcome, got %q", hits[0].Chunk.Text)
	}
}
