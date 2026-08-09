package overview

import (
	"os"
	"path/filepath"
	"strings"
)

// Arch renders the project's architecture/flow — by EXTRACTING existing diagrams
// from the repo's own docs (README, ARCH.md, …), verbatim. It never synthesizes a
// flow: a generated diagram would hallucinate or go stale, and the runtime topology
// (cli → api → providers) isn't reliably derivable from disk (it's not the import
// graph; an env scan false-positives on commented-out vars). So the rule is
// find-and-quote, not generate — present when documented, absent otherwise.
func Arch(root string) string {
	var b strings.Builder
	for _, f := range archDocs(root) {
		for _, blk := range arrowBlocks(filepath.Join(root, f)) {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(f + "\n" + indentLines(blk, "  "))
		}
	}
	if b.Len() == 0 {
		return "no architecture diagram found in the docs.\n" +
			"add a fenced block with arrows (→) to README.md or ARCH.md to document the flow."
	}
	return "Architecture — diagrams from the docs (verbatim)\n\n" + b.String()
}

// firstDiagram returns the single most relevant architecture diagram (ARCH.md
// preferred, then README) for embedding in /overview, or "" if none is documented.
func firstDiagram(root string) string {
	for _, f := range archDocs(root) {
		if blks := arrowBlocks(filepath.Join(root, f)); len(blks) > 0 {
			return blks[0]
		}
	}
	return ""
}

// archDocs returns the docs most likely to hold an architecture diagram (existing only).
func archDocs(root string) []string {
	var out []string
	for _, f := range []string{
		"ARCH.md", "ARCHITECTURE.md", "README.md",
		"docs/architecture.md", "docs/ARCHITECTURE.md", "docs/arch.md",
	} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			out = append(out, f)
		}
	}
	return out
}

// arrowBlocks returns the fenced blocks in a markdown file that are actually flow
// DIAGRAMS — not code or command lists that merely contain an arrow in a comment.
// Two filters: (1) the fence must be a prose/diagram fence (no language, or
// text/mermaid/ascii) — excludes ```go / ```bash; (2) the block must be
// diagram-SHAPED — box-drawing characters, or short with an arrow on every line.
func arrowBlocks(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var blocks, cur []string
	in := false
	lang := ""
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") {
			if in {
				blk := strings.TrimRight(strings.Join(cur, "\n"), "\n")
				if diagramLang(lang) && isDiagram(blk) {
					blocks = append(blocks, blk)
				}
				cur, in = nil, false
			} else {
				in = true
				lang = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(t, "```")))
			}
			continue
		}
		if in {
			cur = append(cur, ln)
		}
	}
	return blocks
}

func diagramLang(l string) bool {
	switch l {
	case "", "text", "txt", "mermaid", "diagram", "ascii":
		return true
	}
	return false
}

// isDiagram: box-drawing chars are a strong signal; otherwise require a short block
// where every non-empty line is an arrow line (excludes multi-line command lists
// that only have an arrow in one comment).
func isDiagram(blk string) bool {
	if strings.ContainsAny(blk, "└├│▶─┌┐┘╰╮↓") {
		return true
	}
	var lines []string
	for _, ln := range strings.Split(blk, "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) == 0 || len(lines) > 6 {
		return false
	}
	for _, ln := range lines {
		if !strings.ContainsAny(ln, "→⟶") && !strings.Contains(ln, "->") {
			return false
		}
	}
	return true
}

func indentLines(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = pad + ln
	}
	return strings.Join(lines, "\n")
}
