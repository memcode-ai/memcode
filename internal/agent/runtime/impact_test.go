package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/tools"
)

func TestFormatImpactTreeAndTests(t *testing.T) {
	root := "/repo"
	levels := [][]impactNode{
		{
			{path: "/repo/server.go", line: 42, name: "Serve", kind: 6},
			{path: "/repo/server_test.go", line: 10, name: "TestServe", kind: 12},
		},
		{
			{path: "/repo/main.go", line: 5, name: "main", kind: 12},
		},
	}
	out := formatImpact(root, levels, 3, false)
	// Repo-relative paths, level headers, kind labels, and a test tally + tag.
	for _, want := range []string{
		"3 transitive caller(s)", "Level 1 — 2 caller(s) (1 in tests)",
		"server.go:42  Serve  <method>", "server_test.go:10  TestServe  <func>  [test]",
		"Level 2 — 1 caller(s)", "main.go:5  main  <func>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatImpact missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/repo/") {
		t.Errorf("paths should be repo-relative, got absolute:\n%s", out)
	}
}

func TestFormatImpactEmptyAndTruncated(t *testing.T) {
	if out := formatImpact("/repo", nil, 0, false); !strings.Contains(out, "No callers found") {
		t.Errorf("empty impact should say no callers: %q", out)
	}
	out := formatImpact("/repo", [][]impactNode{{{path: "/repo/a.go", line: 1, name: "X", kind: 12}}}, 1, true)
	if !strings.Contains(out, "truncated") {
		t.Errorf("truncated impact should note it: %q", out)
	}
}

func TestSymbolKindLabel(t *testing.T) {
	for kind, want := range map[int]string{6: "method", 12: "func", 23: "struct", 999: "symbol"} {
		if got := symbolKindLabel(kind); got != want {
			t.Errorf("symbolKindLabel(%d) = %q, want %q", kind, got, want)
		}
	}
}

// The impact action validates coordinates and degrades cleanly with no language server (CI).
func TestCodeNavImpactValidationAndDegrade(t *testing.T) {
	s := codeNavSess(t)
	ctx := context.Background()
	if !hasTool(s.toolDefs(), tools.CodeNav) {
		t.Fatal("code_nav should be advertised")
	}
	bad, _ := json.Marshal(tools.CodeNavInput{Action: "impact", Path: "x.go"})
	if r := s.codeNavTool(ctx, bad); !r.isError || !strings.Contains(r.text(), "line") {
		t.Fatalf("impact without coords should error on line/col: %q", r.text())
	}
	// Unsupported language → not-configured message, never a crash.
	css, _ := json.Marshal(tools.CodeNavInput{Action: "impact", Path: "styles.css", Line: 1, Col: 1})
	if r := s.codeNavTool(ctx, css); !r.isError || !strings.Contains(r.text(), "no language server") {
		t.Fatalf("unsupported language should report no server: %q", r.text())
	}
}
