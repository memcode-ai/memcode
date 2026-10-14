package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/observe"
	"github.com/memcode-ai/memcode/internal/sessionlog"
)

// fixtureLine is a minimal, synthetic line in the shape confirmed against a
// live Claude Code transcript at implementation time — NOT a copy of real
// transcript content.
func fixtureLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// Import must extract exactly the Turn-worthy events — a human ask, a real
// git commit, a Write's recoverable full content — and skip everything
// else: housekeeping lines with no "message" field, a tool_result handed
// back as a "user" turn, a non-commit Bash call, an Edit (fragments only,
// no recoverable full content), and a line that isn't even valid JSON.
func TestImportExtractsOnlyTurnWorthyEvents(t *testing.T) {
	ts := "2026-10-10T03:32:14.340Z"
	var body string
	body += fixtureLine(t, map[string]any{"type": "mode", "mode": "normal"}) // no "message" — skipped for free
	body += fixtureLine(t, map[string]any{
		"type": "user", "timestamp": ts,
		"message": map[string]any{"role": "user", "content": "migrate the billing webhook to stripe"},
	})
	body += fixtureLine(t, map[string]any{
		"type": "user", "timestamp": ts,
		"message": map[string]any{"role": "user", "content": []map[string]any{
			{"tool_use_id": "x", "type": "tool_result", "content": "ok", "is_error": false},
		}},
	}) // tool_result riding a "user" turn — not a human ask
	body += fixtureLine(t, map[string]any{
		"type": "assistant", "timestamp": ts,
		"message": map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": `rg "git commit" .`}},
		}},
	}) // mentions "git commit" in a search arg — NOT an actual commit
	body += fixtureLine(t, map[string]any{
		"type": "assistant", "timestamp": ts,
		"message": map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "git commit -m wip"}},
		}},
	})
	body += fixtureLine(t, map[string]any{
		"type": "assistant", "timestamp": ts,
		"message": map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "name": "Edit", "input": map[string]any{"file_path": "a.go", "old_string": "x", "new_string": "y"}},
		}},
	}) // Edit: fragments only, no recoverable full content — skipped
	body += fixtureLine(t, map[string]any{
		"type": "assistant", "timestamp": ts,
		"message": map[string]any{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "name": "Write", "input": map[string]any{"file_path": "b.go", "content": "package b\n"}},
		}},
	})
	body += "not even json\n" // malformed — skipped, never fatal

	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	recs, newOffset, err := Import(path, 0, 1<<20)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if newOffset != int64(len(body)) {
		t.Errorf("newOffset = %d, want %d (whole file consumed)", newOffset, len(body))
	}

	var sawAsk, sawRipgrep, sawCommit, sawEdit, sawWrite bool
	for _, r := range recs {
		switch {
		case r.Kind == sessionlog.KindUserMessage && r.Text == "migrate the billing webhook to stripe":
			sawAsk = true
		case r.Tool == "Bash" && r.Input == `rg "git commit" .`:
			sawRipgrep = true
		case r.Tool == "Bash" && r.Input == "git commit -m wip":
			sawCommit = true
		case r.Tool == "Edit":
			sawEdit = true
		case r.Tool == "Write":
			sawWrite = true
			if r.FileClaim == nil || r.FileClaim.Path != "b.go" {
				t.Errorf("Write record missing/wrong FileClaim: %+v", r.FileClaim)
			}
		}
	}
	if !sawAsk {
		t.Error("the human ask is missing")
	}
	if sawRipgrep {
		t.Error("a Bash call merely mentioning 'git commit' must NOT be extracted")
	}
	if !sawCommit {
		t.Error("the real git commit is missing")
	}
	if sawEdit {
		t.Error("an Edit call must never be extracted (no recoverable full content)")
	}
	if !sawWrite {
		t.Error("the Write call with recoverable content is missing")
	}
	// tool_result-as-user and the malformed line must not have produced
	// stray records — exactly 3 Turn-worthy events above.
	if len(recs) != 3 {
		t.Errorf("recs = %d, want exactly 3: %+v", len(recs), recs)
	}
}

// Import must be incremental: calling it again from the previously returned
// offset must see only what was appended since, never re-parse the whole
// file.
func TestImportIsIncremental(t *testing.T) {
	ts1 := "2026-10-10T03:32:14.340Z"
	ts2 := "2026-10-10T04:00:00.000Z"
	line1 := fixtureLine(t, map[string]any{
		"type": "user", "timestamp": ts1,
		"message": map[string]any{"role": "user", "content": "first ask"},
	})
	line2 := fixtureLine(t, map[string]any{
		"type": "user", "timestamp": ts2,
		"message": map[string]any{"role": "user", "content": "second ask"},
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	if err := os.WriteFile(path, []byte(line1), 0o644); err != nil {
		t.Fatal(err)
	}
	recs1, off1, err := Import(path, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs1) != 1 || recs1[0].Text != "first ask" {
		t.Fatalf("first pass = %+v", recs1)
	}

	// Simulate the external tool appending a new turn.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line2); err != nil {
		t.Fatal(err)
	}
	f.Close()

	recs2, off2, err := Import(path, off1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs2) != 1 || recs2[0].Text != "second ask" {
		t.Fatalf("incremental pass should see ONLY the new line, got %+v", recs2)
	}
	if off2 <= off1 {
		t.Errorf("offset must advance: off1=%d off2=%d", off1, off2)
	}
}

// Discover must find a transcript under the escaped-cwd convention and
// respect $CLAUDE_CONFIG_DIR, but must SKIP a transcript older than maxAge —
// ingestion stays bounded to recent work, never "everything, eventually."
func TestDiscoverEscapesCWDAndBoundsByRecency(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)

	repoRoot := filepath.Join(t.TempDir(), "myrepo")
	projDir := filepath.Join(home, "projects", escapeCWD(repoRoot))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(projDir, "fresh.jsonl")
	stale := filepath.Join(projDir, "stale.jsonl")
	if err := os.WriteFile(fresh, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-maxAge - 24*time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	got := Discover([]string{repoRoot})
	if len(got) != 1 || got[0] != fresh {
		t.Fatalf("Discover = %v, want only %q (stale excluded)", got, fresh)
	}
}

var _ observe.Adapter = Importer{} // compile-time interface conformance check
