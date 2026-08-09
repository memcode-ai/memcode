package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The manager lazily starts a server, opens a file, and answers diagnostics + navigation
// by PATH — driven by the in-binary stub (so no real server is needed). It also degrades
// cleanly (ok=false) for a language with no server on PATH.
func TestManagerRoutesByPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("package w\n\nvar _ = Foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root)
	// Point Go at the in-binary stub server instead of gopls.
	m.servers["go"] = serverSpec{bin: os.Args[0], args: []string{"-test.run=TestNoSuchTest"}, languageID: "go", env: []string{"LSP_TEST_SERVER=1"}}
	defer m.Close()
	ctx := context.Background()

	diags, ok, err := m.Diagnostics(ctx, "x.go")
	if err != nil || !ok {
		t.Fatalf("diagnostics: ok=%v err=%v", ok, err)
	}
	if len(diags) != 1 || diags[0].Message != "undefined: Foo" {
		t.Fatalf("diagnostics content: %+v", diags)
	}
	// Navigation resolves (the stub returns fixed locations) and 1-based line/col are
	// accepted (converted to 0-based on the wire).
	defs, ok, err := m.Definition(ctx, "x.go", 3, 9)
	if err != nil || !ok || len(defs) != 1 {
		t.Fatalf("definition: ok=%v err=%v defs=%+v", ok, err, defs)
	}
	refs, _, _ := m.References(ctx, "x.go", 3, 9)
	if len(refs) != 2 {
		t.Fatalf("references: %+v", refs)
	}
	if h, _, _ := m.Hover(ctx, "x.go", 3, 9); h != "func Foo() int" {
		t.Fatalf("hover: %q", h)
	}

	// A language with no configured/available server degrades to ok=false, not an error.
	if _, ok, err := m.Diagnostics(ctx, "styles.css"); ok || err != nil {
		t.Fatalf("unsupported language should be ok=false, err=nil: ok=%v err=%v", ok, err)
	}
}

// Supported reflects PATH: the stub-backed language is supported; a real binary that
// isn't installed is not (and yields an install hint).
func TestManagerSupported(t *testing.T) {
	m := NewManager(t.TempDir())
	if _, ok := m.Supported("a.rb"); ok {
		t.Error("ruby has no configured server → not supported")
	}
	// python's pyright-langserver is almost certainly not installed in CI.
	if _, ok := m.Supported("a.py"); !ok {
		if hint := m.InstallHint("a.py"); hint != "pyright-langserver" {
			t.Errorf("install hint for python = %q", hint)
		}
	}
}
