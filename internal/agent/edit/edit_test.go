package edit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyUniqueReplace(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", "package a\n\nvar X = 1\n")

	res, err := Apply(context.Background(), root, "a.go", "var X = 1", "var X = 2", false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied || res.Occurrences != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := read(t, root, "a.go"); got != "package a\n\nvar X = 2\n" {
		t.Errorf("content = %q", got)
	}
}

func TestApplyNotFound(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", "package a\n")
	if _, err := Apply(context.Background(), root, "a.go", "missing", "x", false); err == nil {
		t.Fatal("expected error for missing old_string")
	}
}

func TestApplyAmbiguous(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", "x\nx\n")
	if _, err := Apply(context.Background(), root, "a.go", "x", "y", false); err == nil {
		t.Fatal("expected error for ambiguous old_string")
	}
	// replace_all resolves the ambiguity.
	res, err := Apply(context.Background(), root, "a.go", "x", "y", true)
	if err != nil {
		t.Fatalf("Apply replace_all: %v", err)
	}
	if res.Occurrences != 2 || read(t, root, "a.go") != "y\ny\n" {
		t.Errorf("replace_all wrong: %+v / %q", res, read(t, root, "a.go"))
	}
}

func TestApplyCreatesNewFile(t *testing.T) {
	root := t.TempDir()
	res, err := Apply(context.Background(), root, "new/dir/file.txt", "", "hello\n", false)
	if err != nil {
		t.Fatalf("Apply create: %v", err)
	}
	if !res.Created {
		t.Error("expected Created=true")
	}
	if read(t, root, "new/dir/file.txt") != "hello\n" {
		t.Error("new file content wrong")
	}
	// Creating over an existing file is rejected.
	if _, err := Apply(context.Background(), root, "new/dir/file.txt", "", "x", false); err == nil {
		t.Fatal("expected error creating over existing file")
	}
}

func TestApplyNoChange(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", "same\n")
	if _, err := Apply(context.Background(), root, "a.go", "same\n", "same\n", false); err == nil {
		t.Fatal("expected error when edit produces no change")
	}
}

// TestFormatGofmtsGo verifies the auto-formatter rewrites a Go file in place via gofmt
// (always present in the Go toolchain), and is a no-op for an unknown extension.
func TestFormatGofmtsGo(t *testing.T) {
	root := t.TempDir()
	// Badly-formatted but valid Go (gofmt will realign).
	write(t, root, "x.go", "package p\nfunc F( ) int {return  1}\n")
	if tool := Format(context.Background(), root, "x.go"); tool != "gofmt" {
		t.Fatalf("Format ran %q, want gofmt", tool)
	}
	got, _ := os.ReadFile(filepath.Join(root, "x.go"))
	want := "package p\n\nfunc F() int { return 1 }\n"
	if string(got) != want {
		t.Errorf("gofmt result:\n got %q\nwant %q", got, want)
	}
	// Unknown extension → no formatter, no change, no error.
	write(t, root, "notes.xyz", "a  b\n")
	if tool := Format(context.Background(), root, "notes.xyz"); tool != "" {
		t.Errorf("Format on .xyz ran %q, want none", tool)
	}
}

// TestSpanDiffFallback locks the fix for the "hit or miss" edit preview: an edit to a file with
// no git diff (untracked / gitignored / no repo) still yields a unified-diff-shaped preview built
// from the replaced span — so the TUI always renders a diff and the model always sees the change.
func TestSpanDiffFallback(t *testing.T) {
	// An edit OUTSIDE any git repo (a bare temp dir) → git diff is empty → fallback kicks in.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Apply(context.Background(), dir, "f.txt", "beta", "BETA", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Diff == "" {
		t.Fatal("edit with no git diff must still produce a span-diff preview, got empty")
	}
	if !strings.Contains(res.Diff, "-beta") || !strings.Contains(res.Diff, "+BETA") {
		t.Errorf("span diff should show the removed/added lines, got:\n%s", res.Diff)
	}
	if !strings.Contains(res.Diff, "@@ -2,1 +2,1 @@") {
		t.Errorf("span diff hunk should be located at the edited line (2), got:\n%s", res.Diff)
	}
}

func TestDiffSplit(t *testing.T) {
	cases := map[string]int{"": 0, "one": 1, "one\n": 1, "a\nb": 2, "a\nb\n": 2, "a\nb\nc\n": 3}
	for in, want := range cases {
		if got := len(diffSplit(in)); got != want {
			t.Errorf("diffSplit(%q) length = %d, want %d", in, got, want)
		}
	}
}
