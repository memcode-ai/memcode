package observe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/sessionlog"
)

// Load must merge memcode's own native session (read-only, via sessionlog)
// with an external source's recorded observation, tagging each turn with
// correct provenance — and never write anything into .memcode/sessions.
func TestLoadMergesNativeAndExternalTaggedBySource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// One native memcode session.
	w, err := sessionlog.Open(root, "sess_native")
	if err != nil {
		t.Fatal(err)
	}
	w.Append(sessionlog.Record{Kind: sessionlog.KindUserMessage, Text: "fix the parser bug"})
	w.Close()

	// One external observation, written exactly as an importer would.
	ow, err := Open(root, "claude-code", "cc_session_1")
	if err != nil {
		t.Fatal(err)
	}
	if err := ow.Append(Record{Kind: sessionlog.KindUserMessage, Text: "migrate the billing webhook"}); err != nil {
		t.Fatal(err)
	}
	ow.Close()

	turns := Load(ctx, root, "")

	var sawNative, sawExternal bool
	for _, tn := range turns {
		switch {
		case tn.Source == "memcode" && tn.Text == "fix the parser bug":
			sawNative = true
			if tn.SourceSessionID != "sess_native" {
				t.Errorf("native turn session id = %q, want sess_native", tn.SourceSessionID)
			}
		case tn.Source == "claude-code" && tn.Text == "migrate the billing webhook":
			sawExternal = true
			if tn.SourceSessionID != "cc_session_1" {
				t.Errorf("external turn session id = %q, want cc_session_1", tn.SourceSessionID)
			}
		}
	}
	if !sawNative {
		t.Errorf("native turn missing: %+v", turns)
	}
	if !sawExternal {
		t.Errorf("external turn missing: %+v", turns)
	}

	// Nothing external ever lands in .memcode/sessions.
	entries, err := os.ReadDir(filepath.Join(root, ".memcode", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sess_native" {
		t.Errorf(".memcode/sessions must hold ONLY the native session, got %v", entries)
	}

	// The external observation lives in its own namespace, untouched by the
	// native writer.
	if _, err := os.Stat(filepath.Join(root, ".memcode", "observe", "claude-code", "cc_session_1.jsonl")); err != nil {
		t.Errorf("external observation missing from its own namespace: %v", err)
	}
}

// HasAny is the cheap presence check callers use to skip Load entirely when
// nothing has ever been ingested — the common case today.
func TestHasAny(t *testing.T) {
	root := t.TempDir()
	if HasAny(root) {
		t.Error("HasAny should be false before anything is written")
	}
	w, err := Open(root, "codex", "sess_1")
	if err != nil {
		t.Fatal(err)
	}
	w.Append(Record{Kind: sessionlog.KindUserMessage, Text: "x"})
	w.Close()
	if !HasAny(root) {
		t.Error("HasAny should be true after a record is written")
	}
}

// A malformed line in an observation file must be skipped, never fatal to
// the merge — these are unversioned, best-effort formats.
func TestLoadSkipsMalformedLines(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, ".memcode", "observe", "codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"ts":"2026-01-01T00:00:00Z","kind":"user_message","text":"good line"}` + "\n" +
		`not even json` + "\n" +
		`{"ts":"2026-01-02T00:00:00Z","kind":"user_message","text":"another good line"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "sess_x.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	turns := Load(ctx, root, "")
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (malformed line skipped): %+v", len(turns), turns)
	}
}

// Clip must never persist raw text past maxText — belt-and-suspenders with
// focus's own strand-level cap.
func TestClipBoundsText(t *testing.T) {
	long := make([]byte, maxText*4)
	for i := range long {
		long[i] = 'a'
	}
	clipped := Clip(string(long))
	if len([]rune(clipped)) > maxText+1 { // +1 for the trailing ellipsis rune
		t.Errorf("Clip left %d runes, want <= %d", len([]rune(clipped)), maxText+1)
	}
}

// fakeAdapter is a minimal Adapter whose Import pulls from an in-memory
// script — standing in for a real importer (claudecode/codex) so Sync's own
// watermarking/redaction/budget behavior can be tested without a real
// transcript file.
type fakeAdapter struct {
	source  string
	files   []string
	imports map[string][]importCall // path -> calls, in the order Sync is expected to make them
	calls   map[string]int
}

type importCall struct {
	recs      []Record
	newOffset int64
}

func (a *fakeAdapter) Source() string               { return a.source }
func (a *fakeAdapter) Discover(_ []string) []string { return a.files }
func (a *fakeAdapter) Import(path string, fromOffset, maxBytes int64) ([]Record, int64, error) {
	if a.calls == nil {
		a.calls = map[string]int{}
	}
	calls := a.imports[path]
	i := a.calls[path]
	a.calls[path] = i + 1
	if i >= len(calls) {
		return nil, fromOffset, nil // nothing more scripted — no-op
	}
	return calls[i].recs, calls[i].newOffset, nil
}

// Sync must persist a watermark so calling it twice never re-imports
// already-processed content, and must redact every Record's Text before it
// ever touches disk.
func TestSyncIsIncrementalAndRedacts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := "/fake/transcript.jsonl"

	a := &fakeAdapter{
		source: "fakesource",
		files:  []string{path},
		imports: map[string][]importCall{
			path: {
				{recs: []Record{{Kind: sessionlog.KindUserMessage, Text: "the SECRET token is abc123"}}, newOffset: 100},
				{recs: []Record{{Kind: sessionlog.KindUserMessage, Text: "a second ask"}}, newOffset: 200},
			},
		},
	}
	redact := func(s string) string {
		return strings.ReplaceAll(s, "abc123", "***REDACTED***")
	}

	if err := Sync(ctx, root, nil, a, redact); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if err := Sync(ctx, root, nil, a, redact); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	if got := a.calls[path]; got != 2 {
		t.Fatalf("Import called %d times, want exactly 2 (one per Sync call, the watermark advancing each time)", got)
	}

	turns := Load(ctx, root, "")
	var sawFirst, sawSecond bool
	for _, tn := range turns {
		if tn.Source != "fakesource" {
			continue
		}
		if strings.Contains(tn.Text, "abc123") {
			t.Fatalf("a secret value leaked to disk unredacted: %q", tn.Text)
		}
		if strings.Contains(tn.Text, "REDACTED") {
			sawFirst = true
		}
		if tn.Text == "a second ask" {
			sawSecond = true
		}
	}
	if !sawFirst || !sawSecond {
		t.Fatalf("both synced observations should be present: %+v", turns)
	}

	// The watermark file itself should record the file's final offset.
	wm := loadWatermarks(root, "fakesource")
	if wm[path] != 200 {
		t.Fatalf("watermark = %v, want {%q: 200}", wm, path)
	}
}
