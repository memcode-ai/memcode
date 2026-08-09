package runtime

import (
	"fmt"
	"image/color"
	"io"
	"regexp"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/mattn/go-runewidth"

	"github.com/memcode-ai/memcode/internal/theme"
)

// diffTabWidth is how many columns a tab expands to in a rendered diff row. Tabs must
// be expanded to spaces BEFORE measuring/padding: the terminal would otherwise tab-stop-
// expand them at render time (8 cols each), blowing the row past the width we padded to
// and wrapping the colored background onto a second line.
const diffTabWidth = 4

var (
	metaStyle = lipgloss.NewStyle().Faint(true) // muted; Faint only, so theme-agnostic

	ansiReset = "\x1b[0m"
)

// addStyle / delStyle / shellPromptStyle are the colored accents the runtime prints AROUND
// its work — the ⏺ tool marker, the $ shell prompt, and the ✖/▶ status glyphs. The diff BODY
// already recolors per theme (newDiffCtx), but these were frozen globals forcing terminal
// green/red, so the marker above a diff stayed green/red even on a pink/blue/matrix theme (the
// /theme preview looked right because it renders only the body, not the marker). They now read
// the ACTIVE theme per call, like newDiffCtx, so EVERY theme renders its own colors: the
// positive accent is the theme's tool-glyph role, the negative accent its danger role.
func addStyle() lipgloss.Style { return lipgloss.NewStyle().Foreground(toolGlyphColor()) }
func delStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(theme.Active().Palette.Danger))
}

// warnStyle is the partial/caution accent — the theme's Warning (amber) role. Used for a tool
// marker that ran and produced output but ended non-zero (a chained command whose last step
// returned non-zero): not a clean success, not an outright failure.
func warnStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(theme.Active().Palette.Warning))
}

// shellPromptStyle is the direct-shell ($) lane prompt — bold, in the theme's tool-glyph color.
// Output renders raw (no rail), on the TUI's verbatim path via a leading ANSI reset in runShell.
func shellPromptStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(toolGlyphColor())
}

// toolGlyphColor is the active theme's tool-glyph color. An empty ToolGlyph means "terminal
// default green" (ANSI SGR 2) — what aurora and random deliberately want — so we preserve that
// rather than dropping to an uncolored glyph.
func toolGlyphColor() color.Color {
	if g := theme.Active().Palette.ToolGlyph; g != "" {
		return lipgloss.Color(g)
	}
	return lipgloss.Color("2")
}

// diffCtx carries resolved diff rendering state, built once per renderDiff/renderNewFile
// call from the active theme. This avoids holding globals that can't react to theme
// changes and keeps diff colors consistent within a single render pass.
type diffCtx struct {
	addBg     string         // ANSI escape for add background
	delBg     string         // ANSI escape for del background
	addGutter string         // ANSI escape for add gutter foreground
	delGutter string         // ANSI escape for del gutter foreground
	gutterCol lipgloss.Style // style for context gutter
}

func newDiffCtx() diffCtx {
	t := theme.Active()
	return diffCtx{
		addBg:     fmt.Sprintf("\x1b[48;2;%d;%d;%dm", t.Diff.AddBg[0], t.Diff.AddBg[1], t.Diff.AddBg[2]),
		delBg:     fmt.Sprintf("\x1b[48;2;%d;%d;%dm", t.Diff.DelBg[0], t.Diff.DelBg[1], t.Diff.DelBg[2]),
		addGutter: fmt.Sprintf("\x1b[1m\x1b[38;2;%d;%d;%dm", t.Diff.AddGutter[0], t.Diff.AddGutter[1], t.Diff.AddGutter[2]),
		delGutter: fmt.Sprintf("\x1b[1m\x1b[38;2;%d;%d;%dm", t.Diff.DelGutter[0], t.Diff.DelGutter[1], t.Diff.DelGutter[2]),
		gutterCol: lipgloss.NewStyle().Foreground(lipgloss.Color(t.Diff.ContextGutter)),
	}
}

const gutterCols = 8 // " 1234 + " — fixed-width gutter (line no. + marker + spaces)

// diffRow renders ONE diff line as a single full-width background row: a bright-fg
// gutter (line number + +/- marker) and syntax-highlighted code share ONE bg shade.
// The code is truncated to the available width BEFORE highlighting, so the row's
// visible width is known exactly (no ANSI width-measurement) and can never reach the
// terminal edge — the cause of the green/red background wrapping onto a second row.
func diffRow(dc diffCtx, lex chroma.Lexer, gutter, bg, code string, width int) string {
	inner := width - 2*diffMargin // bg block width (gutter + code); a margin of black on EACH side
	if inner < gutterCols+1 {
		inner = gutterCols + 1
	}
	code = expandTabs(code, diffTabWidth)                     // tabs → spaces so the terminal can't tab-expand us past the edge
	code, cells := clampWidth(code, inner-gutterCols)         // truncate by DISPLAY CELLS (tabs expanded, CJK=2), not runes
	visible := gutterCols + cells                             // exact on-screen width — never measured through chroma ANSI
	text := gutter + highlightLine(lex, code)                 // gutter ends with a reset → code gets normal syntax fg
	body := strings.ReplaceAll(text, ansiReset, ansiReset+bg) // re-assert bg after every token reset
	if pad := inner - visible; pad > 0 {
		body += strings.Repeat(" ", pad) // trailing fill inherits the bg
	}
	// leftMargin spaces (no bg) + the bg block. The right margin is the unrendered tail:
	// the row is printed at diffMargin+inner = width-diffMargin cells, so it never reaches
	// the terminal edge — no wrap, and a clean gutter of black on both sides.
	return strings.Repeat(" ", diffMargin) + bg + body + ansiReset
}

// diffMargin is the black gutter left to the left AND right of every diff row, so the
// colored band doesn't run edge-to-edge (and never reaches the wrap boundary).
const diffMargin = 2

// expandTabs replaces tabs with spaces to the next tab stop (CJK-aware columns), so a
// rendered row's measured width matches what the terminal draws — a raw \t would be
// tab-expanded at render time and overflow the width we padded to.
func expandTabs(s string, tw int) string {
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := tw - col%tw
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col += runewidth.RuneWidth(r)
	}
	return b.String()
}

// clampWidth truncates s to at most max DISPLAY CELLS (CJK/wide runes count as 2),
// appending an ellipsis when it cuts, and returns the string plus its exact cell width.
// Cell-accurate (not rune count) so a line of tabs/CJK can't overflow and wrap.
func clampWidth(s string, max int) (string, int) {
	if max <= 0 {
		return "", 0
	}
	if w := runewidth.StringWidth(s); w <= max {
		return s, w
	}
	budget := max - 1 // leave room for the ellipsis
	w := 0
	var b strings.Builder
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > budget {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	b.WriteRune('…')
	return b.String(), w + 1
}

var reHunk = regexp.MustCompile(`@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

const maxDiffLines = 80

// maxNewFilePreview caps how many lines of a CREATED file are echoed to scrollback — a
// glimpse, not the whole file (the marker carries the line count; the path is ⌘-clickable).
const maxNewFilePreview = 20

// renderDiff prints a git unified diff as a colored, line-numbered patch so the
// agent's edits are legible in the terminal. The code itself is syntax-highlighted
// (chroma, by the file's language) instead of a flat green/red wash: added and
// context lines read like editor code; removed lines stay red to signal deletion.
// path drives the lexer; pass "" for plain text. Colors auto-strip off a TTY.
//
// The entire diff is assembled into a buffer and emitted as ONE write so the TUI
// receives it as a single streamMsg — preventing a per-line live-region repaint
// that inserts blank rows between diff lines in inline (no-alt-screen) mode.
func renderDiff(out io.Writer, diff, path string, width int) {
	dc := newDiffCtx()
	lex := lexerFor(path)
	lines := strings.Split(diff, "\n")
	added, removed := 0, 0
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
			added++
		case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---"):
			removed++
		}
	}
	// The marker line (⏺ Update(path)) already names the file; summarize the change.
	delta := fmt.Sprintf("+%d", added)
	if removed > 0 {
		delta += fmt.Sprintf(" -%d", removed)
	}

	var buf strings.Builder
	fmt.Fprintf(&buf, "%s\n", metaStyle.Render("  ⎿  "+delta))

	newLine, printed := 0, 0
	for idx, l := range lines {
		if printed >= maxDiffLines {
			rest := 0 // count the remaining real diff lines (skip headers/hunk markers)
			for _, r := range lines[idx:] {
				if strings.HasPrefix(r, "diff ") || strings.HasPrefix(r, "index ") ||
					strings.HasPrefix(r, "--- ") || strings.HasPrefix(r, "+++ ") || strings.HasPrefix(r, "@@") {
					continue
				}
				rest++
			}
			fmt.Fprintln(&buf, metaStyle.Render(fmt.Sprintf("    … +%d more lines", rest)))
			break
		}
		switch {
		case strings.HasPrefix(l, "diff ") || strings.HasPrefix(l, "index ") ||
			strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ "):
			continue
		case strings.HasPrefix(l, "@@"):
			if m := reHunk.FindStringSubmatch(l); m != nil {
				newLine, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(l, "+"):
			g := dc.addGutter + fmt.Sprintf(" %4d + ", newLine) + ansiReset // 8 cols, bright fg
			fmt.Fprintf(&buf, "%s\n", diffRow(dc, lex, g, dc.addBg, l[1:], width))
			newLine++
			printed++
		case strings.HasPrefix(l, "-"):
			g := dc.delGutter + "      - " + ansiReset // 8 cols, no line number (leaving the file)
			fmt.Fprintf(&buf, "%s\n", diffRow(dc, lex, g, dc.delBg, l[1:], width))
			printed++
		default:
			content := expandTabs(strings.TrimPrefix(l, " "), diffTabWidth)
			content, _ = clampWidth(content, width-2*diffMargin-gutterCols)
			fmt.Fprintf(&buf, "%s%s%s\n", strings.Repeat(" ", diffMargin), dc.gutterCol.Render(fmt.Sprintf(" %4d   ", newLine)), highlightLine(lex, content))
			newLine++
			printed++
		}
	}
	fmt.Fprint(out, buf.String())
}

// renderNewFile prints created-file content as all-additions, syntax-highlighted
// by the file's language so a new file reads like code, not a green block.
// Like renderDiff, the output is buffered and emitted as one write.
func renderNewFile(out io.Writer, content, path string, width int) {
	dc := newDiffCtx()
	lex := lexerFor(path)
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	var buf strings.Builder
	fmt.Fprintf(&buf, "%s\n", metaStyle.Render("  ⎿  Created file ("+nLines(len(lines))+")"))
	row := func(idx int, l string) {
		g := dc.addGutter + fmt.Sprintf(" %4d + ", idx+1) + ansiReset // 8 cols, bright fg
		fmt.Fprintf(&buf, "%s\n", diffRow(dc, lex, g, dc.addBg, l, width))
	}
	// A created file is NEW content, not a change to review — a short head glimpse is enough
	// (the marker gives the line count and the path is ⌘-clickable to open the whole thing).
	// Dumping the full file floods scrollback; cap it like the paste preview.
	show, hidden := lines, 0
	if len(lines) > maxNewFilePreview {
		show, hidden = lines[:maxNewFilePreview], len(lines)-maxNewFilePreview
	}
	for i, l := range show {
		row(i, l)
	}
	if hidden > 0 {
		fmt.Fprintln(&buf, metaStyle.Render(fmt.Sprintf("    … +%d more lines (open the file to see the rest)", hidden)))
	}
	fmt.Fprint(out, buf.String())
}

// RenderThemeSample renders a tiny fixed diff snippet using the ACTIVE theme — the
// real diff add/del backgrounds, gutters, and syntax highlighting — so the /theme
// picker's live preview shows what code review actually looks like, not just chrome.
// It reads the active theme live (newDiffCtx), so it recolors as the picker previews.
func RenderThemeSample(width int) string {
	dc := newDiffCtx()
	lex := lexerFor("sample.go")
	var buf strings.Builder
	ctxRow := func(n int, code string) {
		content := expandTabs(code, diffTabWidth)
		content, _ = clampWidth(content, width-2*diffMargin-gutterCols)
		fmt.Fprintf(&buf, "%s%s%s\n", strings.Repeat(" ", diffMargin), dc.gutterCol.Render(fmt.Sprintf(" %4d   ", n)), highlightLine(lex, content))
	}
	del := func(code string) {
		g := dc.delGutter + "      - " + ansiReset
		fmt.Fprintf(&buf, "%s\n", diffRow(dc, lex, g, dc.delBg, code, width))
	}
	add := func(n int, code string) {
		g := dc.addGutter + fmt.Sprintf(" %4d + ", n) + ansiReset
		fmt.Fprintf(&buf, "%s\n", diffRow(dc, lex, g, dc.addBg, code, width))
	}
	ctxRow(1, "func greet(name string) string {")
	del("\tmsg := \"hi \" + name")
	add(2, "\tmsg := fmt.Sprintf(\"hello, %s!\", name)")
	ctxRow(3, "\treturn msg")
	return strings.TrimRight(buf.String(), "\n")
}

func nLines(n int) string {
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}
