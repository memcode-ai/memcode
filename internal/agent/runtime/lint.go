package runtime

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxHealRounds bounds how many times the completion gate can send the agent back
// to fix a broken edit before letting the turn end — so an error it genuinely
// can't fix can't spin the loop.
const maxHealRounds = 2

// brokenEditNudge re-validates every file edited this turn against its CURRENT
// on-disk content and, if any no longer parse, returns a nudge listing them for
// the model to fix. Returns "" when everything edited still parses (the common
// case → the turn ends normally). Reading from disk means a file the model already
// repaired in a later edit correctly reads as clean.
func (s *Session) brokenEditNudge() string {
	var broken []string
	for path := range s.turn.editedPaths {
		abs, err := safeJoin(s.root, path)
		if err != nil {
			continue
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			continue // deleted/moved — not our concern here
		}
		if w := validateEdit(path, string(content)); w != "" {
			broken = append(broken, w)
		}
	}
	if len(broken) == 0 {
		return ""
	}
	sort.Strings(broken) // stable order regardless of map iteration
	return "Before you finish: a file you edited this turn no longer parses. Fix it (don't leave the " +
		"codebase broken), then continue.\n\n" + strings.Join(broken, "\n\n")
}

// Post-edit validation: a cheap, in-process check that an edit didn't leave the
// file broken, fed straight back to the model so it self-corrects on the next
// iteration instead of building on top of a syntax error. This is the "detect bad
// edits and fix them" loop — kept dependency-free (no subprocess, no linters to
// install): for Go we parse with go/parser, which catches the common failure
// (a botched edit that no longer compiles). Other languages return clean for now;
// the seam is here to add tree-sitter/linters later.
//
// It NEVER reverts the edit — the model fixes forward. It only returns a warning
// string ("" when the file is fine) that the caller appends to the tool result.
func validateEdit(path, content string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return validateGo(path, content)
	}
	return ""
}

// validateGo parses Go source and returns a compact, line-numbered syntax-error
// summary, or "" if it parses. Parsing (not full type-checking) is deliberate: it
// needs no build context, runs in microseconds, and catches the edits that matter
// — unbalanced braces, broken signatures, truncated tokens.
func validateGo(path, content string) string {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, filepath.Base(path), content, parser.SkipObjectResolution)
	if err == nil {
		return ""
	}
	// scanner.ErrorList stringifies as "file:line:col: msg" per error; keep the
	// first few so a cascade of follow-on errors doesn't flood the model.
	msgs := strings.Split(err.Error(), "\n")
	const maxErrs = 3
	if len(msgs) > maxErrs {
		msgs = append(msgs[:maxErrs], fmt.Sprintf("…and %d more", len(msgs)-maxErrs))
	}
	return "⚠ this edit left " + filepath.Base(path) + " with a Go syntax error — FIX IT before moving on:\n" +
		strings.Join(msgs, "\n")
}
