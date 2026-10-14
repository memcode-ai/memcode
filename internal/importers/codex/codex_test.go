package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/observe"
	"github.com/memcode-ai/memcode/internal/sessionlog"
)

// fixtureLine is a minimal, synthetic line in the shape confirmed against a
// live Codex rollout at implementation time — NOT a copy of real transcript
// content.
func fixtureLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// Import must extract exactly the Turn-worthy events — a genuine human ask
// (event_msg/user_message) and a real git commit/push (response_item/
// function_call exec_command) — and skip everything else: an agent_message,
// a non-git exec_command, housekeeping envelope types, and a line that
// isn't even valid JSON.
func TestImportExtractsOnlyTurnWorthyEvents(t *testing.T) {
	ts := "2026-08-30T03:17:06.647Z"
	var body string
	body += fixtureLine(t, map[string]any{"timestamp": ts, "type": "turn_context", "payload": map[string]any{}})
	body += fixtureLine(t, map[string]any{"timestamp": ts, "type": "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "migrate the billing webhook to stripe"}})
	body += fixtureLine(t, map[string]any{"timestamp": ts, "type": "event_msg",
		"payload": map[string]any{"type": "agent_message", "message": "I'll look into it", "phase": "commentary"}})
	body += fixtureLine(t, map[string]any{"timestamp": ts, "type": "response_item",
		"payload": map[string]any{"type": "function_call", "name": "exec_command",
			"arguments": `{"cmd":"rg -n \"git commit\" .","workdir":"/repo"}`}}) // mentions it, isn't one
	body += fixtureLine(t, map[string]any{"timestamp": ts, "type": "response_item",
		"payload": map[string]any{"type": "function_call", "name": "exec_command",
			"arguments": `{"cmd":"git commit -m wip","workdir":"/repo"}`}})
	body += fixtureLine(t, map[string]any{"timestamp": ts, "type": "response_item",
		"payload": map[string]any{"type": "function_call_output", "call_id": "x", "output": "done"}})
	body += "not even json\n" // malformed — skipped, never fatal

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-test.jsonl")
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

	var sawAsk, sawRipgrep, sawCommit, sawAgentMsg bool
	for _, r := range recs {
		switch {
		case r.Kind == sessionlog.KindUserMessage && r.Text == "migrate the billing webhook to stripe":
			sawAsk = true
		case r.Kind == sessionlog.KindUserMessage:
			sawAgentMsg = true // anything else that looks like a user message is wrong
		case r.Input == `rg -n "git commit" .`:
			sawRipgrep = true
		case r.Input == "git commit -m wip":
			sawCommit = true
		}
	}
	if !sawAsk {
		t.Error("the human ask is missing")
	}
	if sawAgentMsg {
		t.Error("an agent_message must never be extracted as a user message")
	}
	if sawRipgrep {
		t.Error("an exec_command merely mentioning 'git commit' must NOT be extracted")
	}
	if !sawCommit {
		t.Error("the real git commit is missing")
	}
	if len(recs) != 2 {
		t.Errorf("recs = %d, want exactly 2: %+v", len(recs), recs)
	}
}

// Import must be incremental: a second call from the previously returned
// offset sees only what was appended since.
func TestImportIsIncremental(t *testing.T) {
	ts1 := "2026-08-30T03:17:06.647Z"
	ts2 := "2026-08-30T04:00:00.000Z"
	line1 := fixtureLine(t, map[string]any{"timestamp": ts1, "type": "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "first ask"}})
	line2 := fixtureLine(t, map[string]any{"timestamp": ts2, "type": "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "second ask"}})

	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-test.jsonl")
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

// Discover must find a rollout under the date-partitioned layout, filter by
// the session's recorded cwd, respect $CODEX_HOME, and SKIP a rollout older
// than maxAge — ingestion stays bounded to recent work.
func TestDiscoverFiltersByCWDAndRecency(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	repoRoot := "/Users/t/myrepo"
	otherRepo := "/Users/t/other"

	writeRollout := func(day time.Time, name, cwd string) string {
		dir := filepath.Join(home, "sessions",
			strconv.Itoa(day.Year()), pad2(int(day.Month())), pad2(day.Day()))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		body := fixtureLine(t, map[string]any{
			"timestamp": day.Format(time.RFC3339), "type": "session_meta",
			"payload": map[string]any{"session_id": "x", "cwd": cwd},
		})
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	now := time.Now()
	fresh := writeRollout(now, "rollout-fresh.jsonl", repoRoot)
	writeRollout(now, "rollout-wrong-cwd.jsonl", otherRepo) // wrong project — must be excluded
	stalePath := writeRollout(now.AddDate(0, 0, -30), "rollout-stale.jsonl", repoRoot)
	old := now.AddDate(0, 0, -30)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatal(err)
	}

	got := Discover([]string{repoRoot})
	if len(got) != 1 || got[0] != fresh {
		t.Fatalf("Discover = %v, want only %q", got, fresh)
	}
}

var _ observe.Adapter = Importer{} // compile-time interface conformance check
