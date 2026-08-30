package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/store"
)

// diagnostics reports a clean Go package and surfaces a real compile error with its
// file:line, via the go build fallback (no gopls required in CI).
func TestGoDiagnostics(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes go build")
	}
	root := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module tmpdiag\n\ngo 1.22\n")
	write("ok.go", "package tmpdiag\n\nfunc Add(a, b int) int { return a + b }\n")

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, root, "auto", permissions.ModeAuto, io.Discard)

	in, _ := json.Marshal(tools.DiagnosticsInput{})
	if r := s.diagnosticsTool(context.Background(), in); r.isError || !strings.Contains(r.text(), "clean") {
		t.Fatalf("clean package should report clean: isErr=%v text=%q", r.isError, r.text())
	}

	// Introduce a type error → diagnostics surfaces it with the file name.
	write("bad.go", "package tmpdiag\n\nvar X int = \"not an int\"\n")
	r := s.diagnosticsTool(context.Background(), in)
	if !r.isError {
		t.Fatal("diagnostics with compiler output should be an error tool result")
	}
	if !strings.Contains(r.text(), "bad.go") {
		t.Fatalf("compile error not surfaced with file: %q", r.text())
	}
}

func TestDiagnosticsAdvertised(t *testing.T) {
	st, _ := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "auto", permissions.ModeAuto, io.Discard)
	if !hasTool(s.toolDefs(), tools.Diagnostics) {
		t.Fatal("diagnostics should be advertised")
	}
}

// TestDiagnosticsMarkerShowsActualErrors: the marker alone ("⏺ Diagnostics(go) · 3 line(s)",
// in red) told the user THAT something failed but never WHAT — the actual diagnostic text
// only ever reached the model (via the tool result), never scrollback. diagResult must now
// print a ⎿ preview of the real diagnostic lines under the marker, same as a failed Bash
// command does.
func TestDiagnosticsMarkerShowsActualErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes go build")
	}
	root := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module tmpdiag\n\ngo 1.22\n")
	write("bad.go", "package tmpdiag\n\nvar X int = \"not an int\"\n")

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var out bytes.Buffer
	s := newSess(st, captureProviderNil{}, root, "auto", permissions.ModeAuto, &out)

	in, _ := json.Marshal(tools.DiagnosticsInput{})
	s.diagnosticsTool(context.Background(), in)

	printed := out.String()
	if !strings.Contains(printed, "line(s)") {
		t.Fatalf("marker line missing from scrollback: %q", printed)
	}
	if !strings.Contains(printed, "bad.go") {
		t.Fatalf("the actual error (mentioning bad.go) never reached scrollback — user sees a red count with no detail: %q", printed)
	}
}
