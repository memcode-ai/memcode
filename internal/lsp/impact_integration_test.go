package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestEnclosingSymbolAgainstGopls drives the REAL gopls (skipped when it isn't installed, so
// CI stays green) to verify that a reference to a symbol is correctly attributed to the
// function that contains it — the mechanic the `impact` call-graph walk is built on.
func TestEnclosingSymbolAgainstGopls(t *testing.T) {
	gopls, err := exec.LookPath("gopls")
	if err != nil {
		if home, e := os.UserHomeDir(); e == nil {
			if p := filepath.Join(home, "go", "bin", "gopls"); fileExists(p) {
				gopls = p
			}
		}
	}
	if gopls == "" {
		t.Skip("gopls not installed; skipping live LSP integration test")
	}

	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module p\n\ngo 1.22\n")
	// A small call graph: A() and B() both call Target(); B() also calls A().
	src := `package p

func Target() int { return 1 }

func A() int { return Target() }

func B() int { return A() + Target() }
`
	write(t, filepath.Join(root, "p.go"), src)

	m := NewManager(root)
	m.servers["go"] = serverSpec{bin: gopls, languageID: "go"}
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// `Target` is declared on line 3 at column 6 (1-based).
	refs, ok, err := m.References(ctx, "p.go", 3, 6)
	if err != nil || !ok {
		t.Fatalf("references: ok=%v err=%v", ok, err)
	}
	if len(refs) < 3 { // declaration + call in A + call in B
		t.Fatalf("expected >=3 references to Target, got %d: %+v", len(refs), refs)
	}

	// Each non-declaration reference must attribute to the function that contains it.
	got := map[string]bool{}
	for _, r := range refs {
		if URIToPath(r.URI) != filepath.Join(root, "p.go") {
			continue
		}
		enc, ok, err := m.EnclosingSymbol(ctx, "p.go", r.Range.Start.Line+1, r.Range.Start.Character+1)
		if err != nil || !ok {
			continue
		}
		got[enc.Name] = true
	}
	// The call sites live in A and B (Target's own declaration attributes to Target).
	if !got["A"] || !got["B"] {
		t.Fatalf("enclosing-symbol attribution missing a caller: got %v, want A and B", got)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
