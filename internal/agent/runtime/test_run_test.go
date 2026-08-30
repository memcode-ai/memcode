package runtime

import (
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

// run_tests parses go test -json into a structured summary: counts + the failing test's
// name and output. Runs a real one-pass/one-fail module.
func TestRunTestsGoStructured(t *testing.T) {
	if testing.Short() {
		t.Skip("nested go test")
	}
	root := t.TempDir()
	must := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("go.mod", "module tmptest\n\ngo 1.22\n")
	must("x_test.go", `package tmptest
import "testing"
func TestPasses(t *testing.T) {}
func TestFails(t *testing.T) { t.Fatal("boom-marker") }
`)

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, root, "auto", permissions.ModeAuto, io.Discard)

	in, _ := json.Marshal(tools.RunTestsInput{})
	r := s.runTestsTool(context.Background(), in)
	if !r.isError {
		t.Fatal("failing test run should be an error tool result")
	}
	out := r.text()
	if !strings.Contains(out, "1 passed, 1 failed") {
		t.Fatalf("summary wrong: %q", out)
	}
	if !strings.Contains(out, "TestFails") {
		t.Errorf("failing test not named: %q", out)
	}
	if !strings.Contains(out, "boom-marker") {
		t.Errorf("failure output not surfaced: %q", out)
	}
	// A run with no failures marks verify OK and reports clean.
	in2, _ := json.Marshal(tools.RunTestsInput{Run: "TestPasses"})
	r2 := s.runTestsTool(context.Background(), in2)
	if r2.isError {
		t.Fatalf("passing test run should not be an error: %q", r2.text())
	}
	if !strings.Contains(r2.text(), "1 passed, 0 failed") {
		t.Fatalf("filtered run wrong: %q", r2.text())
	}
}

func TestRunTestsAdvertised(t *testing.T) {
	st, _ := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "auto", permissions.ModeAuto, io.Discard)
	if !hasTool(s.toolDefs(), tools.RunTests) {
		t.Fatal("run_tests should be advertised")
	}
}
