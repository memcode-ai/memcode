package runtime

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// resolveReadPath (read_file / list_dir) honors absolute paths as-is and lets relative paths
// reach outside the repo — mirroring cat/ls. Reads are not repo-scoped (bash can read anywhere
// anyway); only writes stay scoped via safeJoin.
func TestResolveReadPath(t *testing.T) {
	root := t.TempDir()
	if got := resolveReadPath(root, "/etc/hosts"); got != "/etc/hosts" {
		t.Errorf("absolute path should pass through as-is, got %q", got)
	}
	if got := resolveReadPath(root, "a/b.txt"); got != filepath.Join(root, "a/b.txt") {
		t.Errorf("relative path should resolve under root, got %q", got)
	}
	// A `../` relative read is allowed to escape (cat would read it) — no error, no re-rooting.
	if got := resolveReadPath(root, "../sibling.txt"); got != filepath.Join(filepath.Dir(root), "sibling.txt") {
		t.Errorf("relative escape should resolve outside root, got %q", got)
	}
}

// read_file can now read a file OUTSIDE the repo (the bug: an out-of-repo absolute path was
// silently re-rooted under repo and reported as a misleading "not found").
func TestReadFileReachesOutsideRepo(t *testing.T) {
	repo := t.TempDir()
	external := filepath.Join(t.TempDir(), "outside.txt") // a different temp tree, not under repo
	if err := os.WriteFile(external, []byte("hello from outside the repo"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Session{root: repo, out: io.Discard}
	in, _ := json.Marshal(map[string]string{"path": external})
	tr := s.readFile(in)
	if tr.isError {
		t.Fatalf("read_file should read an out-of-repo absolute path, got error: %s", tr.blocks[0].Text)
	}
	if len(tr.blocks) == 0 || tr.blocks[0].Text != "hello from outside the repo" {
		t.Errorf("unexpected content: %+v", tr.blocks)
	}
}

// TestReadFileRange: an optional 1-based inclusive line range returns just that
// slice with a single header line naming it — the body stays UN-numbered so
// edit_file anchors copied from a read never carry line-number poison — and the
// stale-edit guard still hashes the WHOLE file (a range read must not arm it
// with a partial hash).
func TestReadFileRange(t *testing.T) {
	repo := t.TempDir()
	body := "l1\nl2\nl3\nl4\nl5\n"
	path := filepath.Join(repo, "f.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Session{root: repo, out: io.Discard}

	in, _ := json.Marshal(map[string]any{"path": "f.txt", "start_line": 2, "end_line": 4})
	tr := s.readFile(in)
	if tr.isError {
		t.Fatalf("range read errored: %+v", tr.blocks)
	}
	got := tr.blocks[0].Text
	if want := "[lines 2-4 of 5 — f.txt]\nl2\nl3\nl4\n"; got != want {
		t.Fatalf("range read = %q, want %q", got, want)
	}
	// Whole-file hash noted — matches a full read's hash, so the stale-edit
	// guard sees the same content identity either way.
	s2 := &Session{root: repo, out: io.Discard}
	inFull, _ := json.Marshal(map[string]string{"path": "f.txt"})
	s2.readFile(inFull)
	full, ok1 := s2.readHash("f.txt")
	rangeHash, ok2 := s.readHash("f.txt")
	if !ok1 || !ok2 || full != rangeHash {
		t.Fatalf("range read must hash the whole file (full=%q range=%q)", full, rangeHash)
	}

	// Open-ended range clamps to EOF; a range past EOF errors.
	in2, _ := json.Marshal(map[string]any{"path": "f.txt", "start_line": 4})
	if got := s.readFile(in2).blocks[0].Text; got != "[lines 4-5 of 5 — f.txt]\nl4\nl5\n" {
		t.Fatalf("open-ended range = %q", got)
	}
	in3, _ := json.Marshal(map[string]any{"path": "f.txt", "start_line": 99})
	if tr := s.readFile(in3); !tr.isError {
		t.Fatal("start_line past EOF should error")
	}
}

func TestSafeJoinContainment(t *testing.T) {
	root := t.TempDir()

	// Paths that stay inside the repo. A leading-slash path is re-rooted ONLY when
	// the re-rooted parent exists (the "/cli/foo.go means repo-relative" idiom).
	if err := os.MkdirAll(filepath.Join(root, "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a/b.go", ".", "a/../b", "/cli/foo.go"} {
		if _, err := safeJoin(root, p); err != nil {
			t.Errorf("safeJoin(%q) unexpected error: %v", p, err)
		}
	}

	// Paths that escape the repo root must be rejected — including an absolute
	// path whose re-rooted parent doesn't exist ("/etc/passwd" used to be silently
	// fabricated as "<root>/etc/passwd"; the bash-cwd variant of that fabrication
	// surfaced as "fork/exec /bin/sh: no such file or directory").
	for _, p := range []string{"../etc/passwd", "../../.ssh/id_rsa", "../" + filepath.Base(root) + "x/secret", "/etc/passwd", "/Users/nobody/Desktop"} {
		if _, err := safeJoin(root, p); err == nil {
			t.Errorf("safeJoin(%q) expected an error (escapes root)", p)
		}
	}
}

func TestNewlyChanged(t *testing.T) {
	got := newlyChanged([]string{"a", "b"}, []string{"a", "b", "c", "d"})
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("newlyChanged = %v, want [c d]", got)
	}
	if n := newlyChanged([]string{"a"}, []string{"a"}); len(n) != 0 {
		t.Errorf("expected no newly-changed, got %v", n)
	}
}

// TestReadFilePDFBecomesDocumentBlock: reading a .pdf attaches the file as a
// native document block (the model reads it on the LLM call) instead of feeding
// the raw bytes to the model as mojibake text.
func TestReadFilePDFBecomesDocumentBlock(t *testing.T) {
	repo := t.TempDir()
	pdf := []byte("%PDF-1.4\nfake pitch deck body")
	if err := os.WriteFile(filepath.Join(repo, "deck.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Session{root: repo, out: io.Discard}
	in, _ := json.Marshal(map[string]string{"path": "deck.pdf"})
	tr := s.readFile(in)
	if tr.isError {
		t.Fatalf("pdf read errored: %+v", tr.blocks)
	}
	var doc *wire.Block
	for i := range tr.blocks {
		if tr.blocks[i].Type == "document" {
			doc = &tr.blocks[i]
		}
	}
	if doc == nil || doc.Source == nil || doc.Source.MediaType != "application/pdf" {
		t.Fatalf("expected a document block with application/pdf, got %+v", tr.blocks)
	}
	if decoded, _ := base64.StdEncoding.DecodeString(doc.Source.Data); string(decoded) != string(pdf) {
		t.Fatal("document block data does not round-trip the pdf bytes")
	}
}
