package runtime

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"

	"github.com/memcode-ai/memcode/internal/theme"
)

// Syntax highlighting for rendered diffs/new files. We tokenize with chroma and
// emit truecolor ANSI so an edit reads like code in an editor — keywords, strings,
// types in distinct colors — instead of one flat green/red wash. Best-effort: if
// the language is unknown or anything fails, callers fall back to the plain text.

var hlFormatter = formatters.TTY16m // 24-bit truecolor terminal formatter

// lexerFor resolves a chroma lexer from a file path (by extension/name). Returns
// nil when the language isn't recognized — the caller then prints code verbatim.
func lexerFor(path string) chroma.Lexer {
	if path == "" {
		return nil
	}
	l := lexers.Match(path)
	if l == nil {
		// Match() keys off the base name; fall back to an extension-name lookup.
		if i := strings.LastIndex(path, "."); i >= 0 {
			l = lexers.Get(path[i+1:])
		}
	}
	if l == nil {
		return nil
	}
	return chroma.Coalesce(l)
}

// highlightLine syntax-colors a SINGLE physical line of source with the given
// lexer and returns ANSI. It deliberately strips any trailing newline the
// formatter adds so the result is one clean line for the diff gutter layout.
// On any failure it returns the input unchanged.
func highlightLine(lex chroma.Lexer, code string) string {
	if lex == nil || code == "" {
		return code
	}
	it, err := lex.Tokenise(nil, code)
	if err != nil {
		return code
	}
	var b strings.Builder
	if err := hlFormatter.Format(&b, theme.ChromaStyle(), it); err != nil {
		return code
	}
	return strings.TrimRight(b.String(), "\n")
}
