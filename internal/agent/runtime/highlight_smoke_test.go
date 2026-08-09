package runtime

import (
	"strings"
	"testing"
)

func TestHighlightSmoke(t *testing.T) {
	lex := lexerFor("foo.go")
	if lex == nil {
		t.Fatal("no go lexer")
	}
	out := highlightLine(lex, "func main() { return }")
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected ANSI color codes, got %q", out)
	}
	if strings.Contains(out, "\n") {
		t.Errorf("highlightLine must be single-line, got newline: %q", out)
	}
	// Unknown language → plain passthrough.
	if got := highlightLine(lexerFor("foo.unknownext"), "x"); got != "x" {
		t.Errorf("unknown lexer should passthrough, got %q", got)
	}
	// Empty path → nil lexer → passthrough.
	if got := highlightLine(lexerFor(""), "y"); got != "y" {
		t.Errorf("empty path should passthrough, got %q", got)
	}
}
