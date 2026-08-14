package runtime

import (
	"fmt"
	"path/filepath"
	"strings"
)

// toolLine renders EVERY tool-activity marker in ONE shape — "GLYPH Verb(arg) · status"
// — so the surface is consistent instead of each handler inventing its own string. shown is
// the single signal that decides scrollback vs. nothing:
//
//	shown=true  (⏺) — actions the user should SEE: Bash, Update/Write, Web Search,
//	                  Fetch, Explore, Todos, AskUser. Printed to scrollback.
//	shown=false (●) — read-only research: Read, List, Search, Glob, RepoMap, CodeQuery,
//	                  Memcode. Internal housekeeping, not user-facing signal — NOT printed at
//	                  all (the old bubbletea TUI rolled these into the spinner instead of
//	                  scrollback; vxui has no such rollup, so it must simply skip them, or
//	                  every research call clutters the transcript with noise the user never
//	                  asked to see).
//
// arg may be empty (e.g. "Update Todos" has no argument) — then no parens are shown.
// status is an optional trailing note ("shell 1", "failed", "unavailable"). For a file
// edit/create line the path is wrapped in an OSC 8 hyperlink so a supporting terminal
// (iTerm2, Ghostty, kitty, WezTerm) lets you ⌘-click it open in the system default app.
// toolStat is a tool marker's outcome: clean success, a partial/caution (ran + produced output
// but ended non-zero — e.g. a chained command whose last step returned non-zero), or a failure.
type toolStat int

const (
	statOK toolStat = iota
	statWarn
	statFail
)

// toolLine is the boolean-outcome shape kept for the many ok/failed call sites; it delegates to
// toolLineStat. Callers that can be partial (Bash) call toolLineStat directly.
func (s *Session) toolLine(shown bool, verb, arg, status string, failed bool) {
	stat := statOK
	if failed {
		stat = statFail
	}
	s.toolLineStat(shown, verb, arg, status, stat)
}

// Three visibility tiers, decided at the call site:
//
//	⏺ loud   (shown=true)  — an action the user should SEE; stays in scrollback.
//	● quiet  (shown=false) — read-only research (Read/List/Search/Glob): internal
//	         housekeeping the user didn't ask to see, so a SUCCESSFUL one prints
//	         nothing. A warning or failure still prints (dim), because a read that
//	         went wrong is signal, not noise.
//	hidden   — truly internal machinery (CodeQuery/RepoMap/Memcode): don't call toolLine
//	         at all. Hiding is the absence of a marker call, never a dropped line here.
func (s *Session) toolLineStat(shown bool, verb, arg, status string, stat toolStat) {
	// Quiet-tier research that succeeded is not user-facing signal — skip it, so
	// Read/List/Search/Glob don't clutter the transcript. Warnings/failures fall
	// through and still render.
	if !shown && stat == statOK {
		return
	}
	mark := "⏺"
	// Color the bullet on-theme: tool-glyph color for success (Faint for the quiet tier,
	// so research stays visually subordinate), Warning for a partial, Danger for a
	// failure — a failed Read must still be noticeable even on the quiet tier.
	g := addStyle()
	if !shown {
		mark = "●"
		g = metaStyle
	}
	switch stat {
	case statWarn:
		g = warnStyle()
	case statFail:
		g = delStyle()
	}
	glyph := g.Render(mark)
	label := verb
	if arg != "" {
		shownArg := clip(arg, 200) // 100 cut explore/search queries mid-thought
		if fileVerbs[verb] {       // Update/Write: arg is a path → make it ⌘-clickable
			shownArg = osc8FileLink(s.root, arg, shownArg)
		}
		label = verb + "(" + shownArg + ")"
	}
	if status != "" {
		label += " " + metaStyle.Render("· "+status)
	}
	s.printf("%s %s\n", glyph, label)
}

// toolResult renders a tool's ⎿ output preview as ONE block: the corner glyph on the
// FIRST line only, continuation lines aligned beneath it (4-column indent, no repeated
// glyph) — so a command's output reads as a single result instead of N stacked ⎿ rows.
// All lines are muted (metaStyle); callers pre-clip each line to one row.
func (s *Session) toolResult(lines []string) {
	for i, line := range lines {
		prefix := "  ⎿  " // Claude-Code spacing: 2 + glyph + 2 → content at col 5
		if i > 0 {
			prefix = "     " // 5-col align under the first line's text — no glyph on continuations
		}
		s.printf("%s\n", metaStyle.Render(prefix+line))
	}
}

// countNoun renders a pluralized result hint for a quiet tool line's status —
// "1 line", "23 lines", "0 matches" — so a Read/List/Search marker shows what it
// found instead of a bare dot.
func countNoun(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// lineCount is the number of text lines in s (a trailing newline doesn't add a
// phantom empty line; an empty string is 0 lines).
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// fileVerbs mark the tool lines whose arg is a file path worth hyperlinking — the
// edit/create markers from exec.go: "Update" an existing file, "Write" a new one.
var fileVerbs = map[string]bool{"Update": true, "Write": true}

// osc8FileLink wraps display in an OSC 8 hyperlink to the file at path (resolved to an
// ABSOLUTE file:// URI against root, so ⌘-click opens the exact file regardless of the
// terminal's cwd). Terminals that support OSC 8 make it clickable; the rest just show
// display unchanged. The escape is zero-width, so the ANSI-aware scrollback wrap still
// measures the line correctly.
func osc8FileLink(root, path, display string) string {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, path)
	}
	uri := "file://" + strings.ReplaceAll(abs, " ", "%20")
	return "\x1b]8;;" + uri + "\x1b\\" + display + "\x1b]8;;\x1b\\"
}
