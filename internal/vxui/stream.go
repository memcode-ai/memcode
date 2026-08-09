package vxui

import (
	"regexp"
	"strings"

	"github.com/memcode-ai/memcode/internal/theme"
)

// This is the useful subset of the old TUI's stream "absorb": markdown rendering, the blank
// line above ⏺ tool blocks, code-fence gutters, |…| tables, prose indent, and reset-termination.
// Thinking-strip is intentionally omitted (the gateway strips thinking). Noise-rollup (the old
// TUI's trick of animating quiet research markers into the spinner instead of scrollback) is
// ALSO omitted here: quiet (●, shown=false) research lines like Read/List DO arrive from
// toolLineStat (agent/runtime) as single dim lines, and styleScrollbackLine mutes the label
// after the bullet — visible but visually subordinate to loud (⏺) activity. Truly internal
// tools (CodeQuery/RepoMap) emit no marker at all.

const sgrReset = "\x1b[0m"

var (
	mdHeader = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$`)
	mdRule   = regexp.MustCompile(`^\s*(-{3,}|\*{3,}|_{3,})\s*$`)
	mdBullet = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdCode   = regexp.MustCompile("`([^`]+?)`")
	mdBold   = regexp.MustCompile(`\*\*([^*]+?)\*\*`)
	mdItalic = regexp.MustCompile(`\*([^*\s][^*]*?)\*`)
	mdFence  = regexp.MustCompile("^\\s*```")
	ansiRe   = regexp.MustCompile("\x1b\\[[0-9;]*m")
)

func stripSGR(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	return ansiRe.ReplaceAllString(s, "")
}

func isCodeFence(line string) bool { return mdFence.MatchString(strings.TrimSpace(stripSGR(line))) }
func isTableRow(line string) bool  { return strings.HasPrefix(strings.TrimSpace(stripSGR(line)), "|") }

// isToolMarker reports whether a line begins a tool-activity block — either glyph the engine
// uses to head one (⏺ for memcode/colored tools, ● for the rest). A ⎿ result sub-line is part
// of the block above it, NOT a new block, so it deliberately isn't a marker.
func isToolMarker(line string) bool {
	t := strings.TrimSpace(stripSGR(line))
	return strings.HasPrefix(t, "⏺") || strings.HasPrefix(t, "●")
}

// isToolCont reports whether a line is a tool-block continuation (a ⎿ result sub-line). It
// belongs to the block above it, so it hugs the marker — no blank between them.
func isToolCont(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(stripSGR(line)), "⎿")
}

// isToolBlockLine reports whether a line is part of a tool block (its marker or a ⎿ result).
func isToolBlockLine(line string) bool { return isToolMarker(line) || isToolCont(line) }

// toolContIndent is the alignment prefix toolResult uses for a ⎿ result's CONTINUATION lines:
// the corner glyph sits on the first line only, continuations align beneath it with no glyph.
// The stream renderer recognizes this so a multi-line result HUGS its ⎿ line instead of a blank
// being wrongly injected before each glyph-less continuation. Keep in sync with toolResult.
const toolContIndent = "     "

// isToolContinuation reports whether a line is a ⎿-result continuation — a glyph-less line
// carrying toolResult's alignment indent. Callers gate this on "the previous line was part of
// the tool block" so ordinary prose is never mistaken for a continuation.
func isToolContinuation(line string) bool {
	return strings.HasPrefix(stripSGR(line), toolContIndent)
}

// startsWithToolGlyph reports whether a streamed chunk begins a discrete tool line (⏺ or ⎿),
// so it can be forced onto its own row instead of gluing onto unterminated prose.
func startsWithToolGlyph(chunk string) bool {
	t := strings.TrimLeft(stripSGR(chunk), " \t")
	return strings.HasPrefix(t, "⏺") || strings.HasPrefix(t, "⎿")
}

func renderCodeLine(line string) string {
	p := theme.Active().Palette
	return ansiSGR(p.Dim, false) + "▏ " + sgrReset + ansiSGR(p.Muted, false) + line + sgrReset
}

// mdToANSI renders one line of light markdown to ANSI. Pre-styled lines (diffs) pass through.
func mdToANSI(line string) string {
	if strings.ContainsRune(line, 0x1b) {
		return line
	}
	p := theme.Active().Palette
	if mdRule.MatchString(line) {
		return ansiSGR(p.Dim, false) + strings.Repeat("─", 24) + sgrReset
	}
	if strings.HasPrefix(strings.TrimSpace(line), "|") {
		cells := tableCells(line)
		if isTableSeparator(cells) {
			return ""
		}
		return mdInline(strings.Join(cells, "   "))
	}
	if m := mdHeader.FindStringSubmatch(line); m != nil {
		return ansiSGR(p.Brand, true) + mdInline(m[1]) + sgrReset
	}
	if m := mdBullet.FindStringSubmatch(line); m != nil {
		return m[1] + "• " + mdInline(m[2])
	}
	return mdInline(line)
}

func mdInline(s string) string {
	p := theme.Active().Palette
	s = mdCode.ReplaceAllStringFunc(s, func(m string) string {
		return ansiSGR(p.Secondary, false) + strings.Trim(m, "`") + sgrReset
	})
	s = mdBold.ReplaceAllStringFunc(s, func(m string) string {
		return "\x1b[1m" + strings.Trim(m, "*") + "\x1b[22m"
	})
	s = mdItalic.ReplaceAllStringFunc(s, func(m string) string {
		return "\x1b[3m" + strings.Trim(m, "*") + "\x1b[23m"
	})
	return s
}

func tableCells(line string) []string {
	var cells []string
	for _, c := range strings.Split(strings.TrimSpace(line), "|") {
		if c = strings.TrimSpace(c); c != "" {
			cells = append(cells, c)
		}
	}
	return cells
}

func isTableSeparator(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if strings.Trim(c, ":- ") != "" {
			return false
		}
	}
	return true
}

// styleScrollbackLine styles a kept line: muted ● markers stay flush-left; pre-styled lines
// (⏺ tool markers, diffs) pass through; prose is indented two spaces and markdown-rendered.
func styleScrollbackLine(line string) string {
	if strings.HasPrefix(strings.TrimSpace(stripSGR(line)), "●") {
		// Quiet research line. The runtime colors the leading ● bullet on-theme (same accent as
		// the loud ⏺ marker); keep that colored bullet and mute only the label after it, so the
		// quiet/loud hierarchy stays without forcing the bullet gray. A plain ● with no SGR
		// (older path / tests) falls back to muting the whole line.
		if i := strings.Index(line, sgrReset); i >= 0 {
			head, tail := line[:i+len(sgrReset)], line[i+len(sgrReset):]
			return head + ansiSGR(theme.Active().Palette.Muted, false) + tail + sgrReset
		}
		return ansiSGR(theme.Active().Palette.Muted, false) + line + sgrReset
	}
	if strings.ContainsRune(line, 0x1b) {
		return line
	}
	return "  " + mdToANSI(line)
}

// absorbOutput turns a streamed engine chunk into finished scrollback lines (or "" if the line
// isn't complete yet). It owns the cross-chunk buffers via appState (appendBuf, inCodeFence,
// lastBlank). The returned text has no trailing newline; callers add one.
func (s *appState) absorbOutput(chunk string) string {
	if startsWithToolGlyph(chunk) {
		if b := s.appendBuf.String(); b != "" && !strings.HasSuffix(b, "\n") {
			s.appendBuf.WriteString("\n") // de-merge: tool line starts its own row
		}
	}
	s.appendBuf.WriteString(chunk)
	full := s.appendBuf.String()
	nl := strings.LastIndexByte(full, '\n')
	if nl < 0 {
		return ""
	}
	lines := strings.Split(full[:nl], "\n")
	s.appendBuf.Reset()
	s.appendBuf.WriteString(full[nl+1:])

	// A tool block (a ⏺/● marker plus its ⎿ result lines) is isolated by ONE blank line on each
	// side — above when entering it (prose/another block → marker) and below when leaving it
	// (block → prose). Within the block, marker and ⎿ lines hug. prevBlock/prevBlank track the
	// previous emitted CONTENT line across the chunk boundary (seeded from s.lastTool/lastBlank).
	var keep []string
	prevBlock, prevBlank := s.lastTool, s.lastBlank
	for _, ln := range lines {
		if isCodeFence(ln) {
			s.inCodeFence = !s.inCodeFence
			continue // drop the ``` marker
		}
		if s.inCodeFence {
			keep = append(keep, renderCodeLine(ln))
			prevBlock, prevBlank = false, false
			continue
		}
		if strings.TrimSpace(stripSGR(ln)) == "" {
			keep = append(keep, ln) // preserve blanks for spacing
			prevBlock, prevBlank = false, true
			continue
		}
		// A ⎿ result's continuation lines carry only the alignment indent (no ⎿ glyph); treat them
		// as part of the block above so the result hugs its ⎿ line instead of a blank being injected
		// before the first continuation (the bug: glyph-less continuations read as "leaving" the block).
		block := isToolBlockLine(ln) || (prevBlock && isToolContinuation(ln))
		enter := isToolMarker(ln) && !prevBlank // crossing INTO a tool block
		leave := !block && prevBlock            // crossing OUT of one into prose
		if (enter || leave) && len(keep) > 0 {
			keep = append(keep, "") // within-chunk separator; the boundary case is handled below
		}
		keep = append(keep, styleScrollbackLine(ln))
		prevBlock, prevBlank = block, false
	}
	for i, ln := range keep {
		if strings.ContainsRune(ln, 0x1b) && !strings.HasSuffix(ln, sgrReset) {
			keep[i] = ln + sgrReset // reset-terminate so color can't bleed into the next row
		}
	}
	out := strings.TrimLeft(strings.Join(keep, "\n"), "\n")
	if strings.TrimSpace(stripSGR(out)) == "" {
		return ""
	}
	// Chunk boundary: separate this chunk's first content line from the previous scrollback when
	// it crosses a tool-block edge (entering a block, or leaving the previous chunk's block).
	first := out
	if j := strings.IndexByte(out, '\n'); j >= 0 {
		first = out[:j]
	}
	// A continuation as the chunk's FIRST line (the prev chunk ended inside the block) hugs too.
	firstBlock := isToolBlockLine(first) || (s.lastTool && isToolContinuation(first))
	enterFirst := isToolMarker(first) && !s.lastBlank
	leaveFirst := !firstBlock && s.lastTool && !s.lastBlank
	if enterFirst || leaveFirst {
		out = "\n" + out
	}
	s.lastBlank = strings.HasSuffix(out, "\n")
	s.lastTool = prevBlock // block-ness of the last content line (⎿ continuations included), carried across chunks
	return out
}
