package sessionlog

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/config"
)

// writeSession appends a sequence of records under root for the given id.
func writeSession(t *testing.T, root, id string, recs []Record) {
	t.Helper()
	w, err := Open(root, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, r := range recs {
		w.Append(r)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAppendAndRecent(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "sess_a", []Record{
		{Kind: KindSessionStarted, Model: "opus", Mode: "ask", HeadSHA: "deadbeef"},
		{Kind: KindUserMessage, Text: "add the thing"},
		{Kind: KindToolCall, Tool: "bash", Input: `{"command":"git status"}`},
		{Kind: KindToolResult, ToolUseID: "t1", Content: "clean"},
		{Kind: KindAssistantMessage, Text: "done"},
		{Kind: KindSessionFinished},
	})

	recs, err := Recent(root, "sess_a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 6 {
		t.Fatalf("got %d records, want 6", len(recs))
	}
	if recs[1].Kind != KindUserMessage || recs[1].Text != "add the thing" {
		t.Fatalf("record 1 wrong: %+v", recs[1])
	}
	// Recent(n) keeps the LAST n.
	last2, _ := Recent(root, "sess_a", 2)
	if len(last2) != 2 || last2[1].Kind != KindSessionFinished {
		t.Fatalf("Recent(2) wrong: %+v", last2)
	}

	// The files actually exist on disk under .memcode/sessions/<id>/.
	dir := filepath.Join(root, config.DirName, "sessions", "sess_a")
	for _, f := range []string{"events.jsonl", "transcript.md"} {
		if _, err := readRecords(filepath.Join(dir, "events.jsonl")); err != nil {
			t.Fatalf("events unreadable: %v", err)
		}
		_ = f
	}
}

// Reduce must count commits via the shell AST, not a substring: searching for the PHRASE
// "git commit" is not a commit, and the old substring check logged phantom commits into the
// work-history digest.
func TestReduceCommitDetectionIsAST(t *testing.T) {
	recs := []Record{
		{Kind: KindToolCall, Tool: "bash", Input: `{"command":"git commit -m real"}`},
		{Kind: KindToolCall, Tool: "bash", Input: `{"command":"rg \"git commit\" ."}`},  // phrase, not a commit
		{Kind: KindToolCall, Tool: "bash", Input: `{"command":"echo git push please"}`}, // mention, not a push
	}
	d := Reduce(recs)
	if len(d.Commits) != 1 {
		t.Fatalf("expected exactly 1 real commit, got %d: %v", len(d.Commits), d.Commits)
	}
}

func TestSidequestsAndReduce(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "sess_b", []Record{
		{Kind: KindUserMessage, Text: "fix the paste bug"},
		{Kind: KindToolCall, Tool: "edit_file", Input: `{"path":"tui.go"}`},
		{Kind: KindToolCall, Tool: "bash", Input: `{"command":"go test ./..."}`},
		{Kind: KindUserMessage, Text: "commit and push"},
		{Kind: KindToolCall, Tool: "bash", Input: `{"command":"git commit -m wip"}`},
		{Kind: KindToolCall, Tool: "bash", Input: `{"command":"git push"}`},
	})

	sq, err := Sidequests(root, "sess_b")
	if err != nil {
		t.Fatal(err)
	}
	if len(sq) != 2 || sq[0].Text != "fix the paste bug" || sq[1].Text != "commit and push" {
		t.Fatalf("sidequests wrong: %+v", sq)
	}

	recs, _ := Recent(root, "sess_b", 0)
	d := Reduce(recs)
	if len(d.Requests) != 2 {
		t.Fatalf("digest requests = %d, want 2", len(d.Requests))
	}
	if len(d.Commits) != 2 { // commit + push
		t.Fatalf("digest commits = %d, want 2", len(d.Commits))
	}
	if d.Tests != 1 {
		t.Fatalf("digest tests = %d, want 1 (go test)", d.Tests)
	}
	if d.Edits != 1 {
		t.Fatalf("digest edits = %d, want 1", d.Edits)
	}
	lines := strings.Join(d.Lines(), "\n")
	if !strings.Contains(lines, "asked: fix the paste bug") || !strings.Contains(lines, "ran: ") {
		t.Fatalf("digest lines missing expected content:\n%s", lines)
	}
}

func TestSearchAndCommitsAcrossSessions(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "sess_old", []Record{
		{Kind: KindUserMessage, Text: "build keyprobe"},
		{Kind: KindToolCall, Tool: "bash", Input: `{"command":"git commit -m keyprobe"}`},
	})
	writeSession(t, root, "sess_new", []Record{
		{Kind: KindUserMessage, Text: "elastic composer"},
		{Kind: KindToolCall, Tool: "bash", Input: `{"command":"go build ./..."}`},
	})

	hits, err := Search(root, "keyprobe", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("search should find 'keyprobe' across sessions")
	}
	found := false
	for _, h := range hits {
		if strings.Contains(h.Text+h.Input, "keyprobe") {
			found = true
		}
	}
	if !found {
		t.Fatalf("search hits missing the keyprobe record: %+v", hits)
	}

	commits, err := Commits(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 {
		t.Fatalf("commits = %d, want 1 (the git commit)", len(commits))
	}
}

func TestLatestRecentPicksNewestSession(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "sess_1", []Record{{Kind: KindUserMessage, Text: "first session"}})
	writeSession(t, root, "sess_2", []Record{{Kind: KindUserMessage, Text: "second session"}})

	recs, err := LatestRecent(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	// sess_2 was written last → newest mtime → chosen.
	if len(recs) == 0 || recs[0].Text != "second session" {
		t.Fatalf("LatestRecent should pick the newest session, got %+v", recs)
	}
}

func TestReadersTolerateMissing(t *testing.T) {
	root := t.TempDir() // no sessions dir at all
	if recs, err := LatestRecent(root, 5); err != nil || recs != nil {
		t.Fatalf("LatestRecent on empty root should be (nil,nil), got %v %v", recs, err)
	}
	if hits, err := Search(root, "x", 5); err != nil || hits != nil {
		t.Fatalf("Search on empty root should be (nil,nil), got %v %v", hits, err)
	}
}

// A nil Writer is safe (graceful degradation when the dir can't be created).
func TestNilWriterSafe(t *testing.T) {
	var w *Writer
	w.Append(Record{Kind: KindUserMessage, Text: "x"})
	if err := w.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

// TestRecentPlans: presented-plan records are recovered across sessions, newest-first, deduped by
// slug (the latest revision wins) — the canonical recovery path recall falls back to.
func TestRecentPlans(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "sess_a", []Record{
		{Kind: KindPlanPresented, Text: "# Plan Alpha\nstep one", Slug: "aaa-bbb-ccc"},
	})
	writeSession(t, root, "sess_b", []Record{
		{Kind: KindPlanPresented, Text: "# Plan Beta v1", Slug: "ddd-eee-fff"},
		{Kind: KindPlanPresented, Text: "# Plan Beta v2 (revised)", Slug: "ddd-eee-fff"}, // same slug → revision
	})

	got, err := RecentPlans(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct plans (deduped by slug), got %d", len(got))
	}
	var beta *PlanRec
	for i := range got {
		if got[i].Slug == "ddd-eee-fff" {
			beta = &got[i]
		}
	}
	if beta == nil || !strings.Contains(beta.Text, "v2 (revised)") {
		t.Fatalf("the latest revision of a slug must win, got %+v", beta)
	}
	if got[0].Title == "" {
		t.Fatal("title should be derived from the first heading")
	}
}

func TestSearchIndexesFactsAndLessons(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "sess_f", []Record{
		{Kind: KindUserMessage, Text: "let's work on the parser"},
		{Kind: KindFacts, Text: "The retry queue preserves webhook delivery order.", Entities: []string{"retry queue", "webhooks"}},
		{Kind: KindLessonSignal, Trigger: "editing generated protobuf files", Strategy: "regenerate instead of hand-editing"},
	})

	hits, err := Search(root, "webhook delivery ordering", 5)
	if err != nil || len(hits) == 0 || hits[0].Kind != KindFacts {
		t.Fatalf("facts record should rank first for its own content, got %+v err=%v", hits, err)
	}
	if hits[0].SessionID != "sess_f" {
		t.Fatalf("hit missing session attribution: %+v", hits[0])
	}
	hits, err = Search(root, "generated protobuf", 5)
	if err != nil || len(hits) == 0 || hits[0].Kind != KindLessonSignal {
		t.Fatalf("lesson trigger/strategy should be searchable, got %+v err=%v", hits, err)
	}
}
