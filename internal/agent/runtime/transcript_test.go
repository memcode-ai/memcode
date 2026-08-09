package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

func persistFor(t *testing.T, root, id string, msgs []wire.Message) {
	t.Helper()
	s := &Session{root: root, sessionID: id}
	s.persistTranscript(&ChatState{messages: msgs})
	if _, err := os.Stat(transcriptPath(root, id)); err != nil {
		t.Fatalf("transcript for %s not written: %v", id, err)
	}
}

func TestTranscriptRoundTrip(t *testing.T) {
	root := t.TempDir()
	msgs := []wire.Message{
		{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "hello"}}},
		{Role: "assistant", Blocks: []wire.Block{
			{Type: "thinking", Thinking: "hmm", Signature: "sig123"}, // must round-trip verbatim
			{Type: "tool_use", ID: "tu_1", Name: "read_file", Input: []byte(`{"path":"a.go"}`)},
		}},
		{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: "tu_1", Content: "package a"}}},
	}
	persistFor(t, root, "sess_aaaa1111", msgs)

	got, err := loadTranscript(root, "sess_aaaa1111")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3", len(got))
	}
	th := got[1].Blocks[0]
	if th.Type != "thinking" || th.Thinking != "hmm" || th.Signature != "sig123" {
		t.Fatalf("thinking block did not round-trip: %+v", th)
	}
	if string(got[1].Blocks[1].Input) != `{"path":"a.go"}` {
		t.Fatalf("tool_use input did not round-trip: %s", got[1].Blocks[1].Input)
	}

	// Empty history is never persisted and never resumable.
	s := &Session{root: root, sessionID: "sess_empty000"}
	s.persistTranscript(&ChatState{})
	if _, err := loadTranscript(root, "sess_empty000"); err == nil {
		t.Fatal("an empty session must not be resumable")
	}
}

func TestResolveSession(t *testing.T) {
	root := t.TempDir()
	msg := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "x"}}}}
	persistFor(t, root, "sess_aaaa1111", msg)
	persistFor(t, root, "sess_bbbb2222", msg)
	// Force mtime ordering: bbbb is newer.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(transcriptPath(root, "sess_aaaa1111"), old, old); err != nil {
		t.Fatal(err)
	}

	for _, ref := range []string{"", "latest"} {
		id, err := ResolveSession(root, ref)
		if err != nil || id != "sess_bbbb2222" {
			t.Fatalf("ResolveSession(%q) = %q, %v — want the newest", ref, id, err)
		}
	}
	if id, err := ResolveSession(root, "aaaa"); err != nil || id != "sess_aaaa1111" {
		t.Fatalf("bare prefix: got %q, %v", id, err)
	}
	if id, err := ResolveSession(root, "sess_bbbb2222"); err != nil || id != "sess_bbbb2222" {
		t.Fatalf("exact id: got %q, %v", id, err)
	}
	if _, err := ResolveSession(root, "zzzz"); err == nil {
		t.Fatal("unknown ref must error")
	}
	if _, err := ResolveSession(root, "sess_"); err == nil {
		t.Fatal("ambiguous prefix must error")
	}
	if ids := ResumableSessions(root); len(ids) != 2 || ids[0] != "sess_bbbb2222" {
		t.Fatalf("ResumableSessions = %v", ids)
	}
	if _, err := ResolveSession(t.TempDir(), "latest"); err == nil {
		t.Fatal("no sessions dir must error")
	}
}

// TestPersistTranscriptSurfacesError verifies that when atomicfile.WriteFile
// fails (the session directory is read-only), the ⚠ warning is emitted to
// s.out — so the user knows the session won't be resumable, instead of the
// failure vanishing silently.
func TestPersistTranscriptSurfacesError(t *testing.T) {
	root := t.TempDir()

	// Create the session dir and make it read-only so atomicfile.WriteFile
	// cannot create the transcript file inside it.
	sessDir := filepath.Join(root, ".memcode", "sessions", "sess_readonly")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sessDir, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sessDir, 0o755) }) // restore so t.TempDir cleanup works

	var buf bytes.Buffer
	s := &Session{root: root, sessionID: "sess_readonly", out: &buf}
	s.persistTranscript(&ChatState{messages: []wire.Message{
		{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "hello"}}},
	}})

	out := buf.String()
	if !strings.Contains(out, "⚠") || !strings.Contains(out, "transcript") {
		t.Fatalf("expected a ⚠ transcript warning in output, got: %s", out)
	}
}

// ForkSession copies the transcript (+ checkpoints) to a fresh id, leaves the
// original byte-identical, and deliberately does NOT duplicate the episodic log
// (recall/orientation would double-count a copied history).
func TestForkSession(t *testing.T) {
	root := t.TempDir()
	msgs := []wire.Message{
		{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "hi"}}},
		{Role: "assistant", Blocks: []wire.Block{{Type: "text", Text: "hello"}}},
	}
	persistFor(t, root, "sess_src11111", msgs)
	// An episodic log + a checkpoint on the source session.
	srcDir := filepath.Join(root, ".memcode", "sessions", "sess_src11111")
	if err := os.WriteFile(filepath.Join(srcDir, "events.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ckDir := filepath.Join(root, ".memcode", "checkpoints", "sess_src11111", "1")
	if err := os.MkdirAll(filepath.Join(ckDir, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ckDir, "manifest.json"), []byte(`{"seq":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	origBytes, _ := os.ReadFile(transcriptPath(root, "sess_src11111"))

	newID, err := ForkSession(root, "sess_src11111")
	if err != nil {
		t.Fatal(err)
	}
	if newID == "sess_src11111" || newID == "" {
		t.Fatalf("fork must mint a NEW id, got %q", newID)
	}
	// The fork loads the same messages under its own id.
	got, err := loadTranscript(root, newID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(msgs) || got[0].Blocks[0].Text != "hi" {
		t.Fatalf("forked transcript differs: %+v", got)
	}
	var forked transcript
	b, _ := os.ReadFile(transcriptPath(root, newID))
	if err := json.Unmarshal(b, &forked); err != nil || forked.SessionID != newID {
		t.Fatalf("fork must carry the NEW id in session_id, got %q (err %v)", forked.SessionID, err)
	}
	// Original untouched, byte for byte.
	if after, _ := os.ReadFile(transcriptPath(root, "sess_src11111")); !bytes.Equal(origBytes, after) {
		t.Fatal("fork mutated the original transcript")
	}
	// Checkpoints ride along; the episodic log does NOT.
	if _, err := os.Stat(filepath.Join(root, ".memcode", "checkpoints", newID, "1", "manifest.json")); err != nil {
		t.Errorf("checkpoints should be copied to the fork: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".memcode", "sessions", newID, "events.jsonl")); !os.IsNotExist(err) {
		t.Error("the episodic log must NOT be duplicated into the fork")
	}

	if _, err := ForkSession(root, "sess_missing"); err == nil {
		t.Error("forking a nonexistent session must error")
	}
}
