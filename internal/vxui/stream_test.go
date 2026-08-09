package vxui

import (
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"
	"github.com/memcode-ai/memcode/internal/theme"
)

func TestMarkdownRendering(t *testing.T) {
	theme.Set("aurora")
	cases := map[string]string{
		"## Heading":    "Heading",
		"**bold** here": "bold here",
		"a `code` span": "a code span",
		"- list item":   "• list item",
		"plain prose":   "plain prose",
	}
	for in, wantPlain := range cases {
		got := mdToANSI(in)
		if stripSGR(got) != wantPlain {
			t.Errorf("mdToANSI(%q) plain = %q, want %q", in, stripSGR(got), wantPlain)
		}
	}
	// headers/bold should actually carry styling
	if !strings.ContainsRune(mdToANSI("## Heading"), 0x1b) {
		t.Error("header rendered without styling")
	}
}

func TestToolBlockSpacing(t *testing.T) {
	theme.Set("aurora")
	s := &appState{}
	out := s.absorbOutput("⏺ Bash(ls)\n")
	if !strings.HasPrefix(out, "\n") {
		t.Errorf("expected a blank line above the ⏺ tool block, got %q", out)
	}
	if !strings.Contains(out, "⏺ Bash(ls)") {
		t.Errorf("tool line missing: %q", out)
	}
}

// Regression: a multi-line ⎿ result must HUG its corner-glyph line. The continuation lines
// carry only toolResult's 5-space alignment indent (no ⎿ glyph); they were misread as "prose"
// so a blank line got injected after the first result row. (The "\n bug".)
func TestToolResultContinuationsHug(t *testing.T) {
	theme.Set("aurora")
	s := &appState{}
	dim := func(x string) string { return "\x1b[2m" + x + "\x1b[0m" } // mimic metaStyle SGR wrap
	chunk := "⏺ Bash(git status)\n" +
		dim("  ⎿  M RUNBOOK.md") + "\n" +
		dim("     M api/README.md") + "\n" +
		dim("     M api/deploy.sh") + "\n"
	body := stripSGR(s.absorbOutput(chunk))
	if strings.Contains(body, "RUNBOOK.md\n\n") || strings.Contains(body, "\n\n     ") {
		t.Errorf("blank line wrongly injected inside the ⎿ result block:\n%q", body)
	}
	for _, want := range []string{"M RUNBOOK.md", "M api/README.md", "M api/deploy.sh"} {
		if !strings.Contains(body, want) {
			t.Errorf("result row %q dropped from:\n%q", want, body)
		}
	}
}

// A quiet (●) research line from the runtime keeps its styled bullet and gets its label
// muted after the first SGR reset — visible but visually subordinate; a plain unstyled ●
// line (older path / tests) is muted whole.
func TestQuietResearchLineMutedNotDropped(t *testing.T) {
	theme.Set("aurora")
	styled := "\x1b[2m●\x1b[0m Read(internal/x.go) · 42 lines"
	out := styleScrollbackLine(styled)
	if !strings.Contains(out, "●") || !strings.Contains(out, "Read(internal/x.go)") {
		t.Fatalf("quiet line must survive styling intact, got %q", out)
	}
	if !strings.Contains(out[strings.Index(out, sgrReset):], "\x1b[") {
		t.Errorf("the label after the bullet should be muted (styled), got %q", out)
	}
	plain := styleScrollbackLine("● foo")
	if !strings.HasPrefix(plain, "\x1b[") || !strings.Contains(plain, "● foo") {
		t.Errorf("a plain ● line should be muted whole, got %q", plain)
	}
}

func TestProseIndentedToolPassthrough(t *testing.T) {
	theme.Set("aurora")
	s := &appState{}
	out := s.absorbOutput("hello world\n")
	if !strings.HasPrefix(stripSGR(out), "  hello world") {
		t.Errorf("prose should be indented two spaces, got %q", stripSGR(out))
	}
}

func TestMatrixIntroBuildsRows(t *testing.T) {
	theme.Set("matrix")
	s := &appState{width: 120, introRecall: "↺ ready", intro: true, introFrame: 30}
	w := s.matrixIntroView()
	fx, ok := w.(ui.Flex)
	if !ok {
		t.Fatalf("matrixIntroView = %T, want ui.Flex", w)
	}
	if len(fx.Children) < 10 {
		t.Fatalf("intro card built %d rows, want ~14", len(fx.Children))
	}
}
