package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/lsp"
)

// Impact-analysis bounds. The call graph is walked breadth-first outward; these caps keep a
// hot symbol (called from hundreds of sites) from flooding context or stalling the turn.
const (
	defaultImpactDepth = 2
	maxImpactDepth     = 3
	maxImpactPerLevel  = 40
	maxImpactTotal     = 120
	// maxCodeNavRefs caps the flat find-references list (a hot symbol can have hundreds).
	maxCodeNavRefs = 200
)

// impactNode is one symbol in the call graph: a function/method that (transitively) reaches
// the target. Identified by file+def-line+name so the same caller is never expanded twice.
type impactNode struct {
	key  string
	path string // absolute
	line int    // 1-based def line (selectionRange) — the position to re-query references from
	col  int
	name string
	kind int
}

// impact answers "if I change this symbol, what breaks?" — the blast radius. It walks the
// call graph outward from the symbol at (path,line,col): its references define the level-1
// callers (attributed to their enclosing function via documentSymbol), those callers'
// references define level-2, and so on up to `depth`. Bounded and truncated. Built entirely
// on the resident LSP (references + documentSymbol), so it's as accurate as the language
// server and degrades to an actionable message when no server is available.
func (s *Session) impact(ctx context.Context, m *lsp.Manager, path string, line, col, depth int) toolResult {
	if depth <= 0 {
		depth = defaultImpactDepth
	}
	if depth > maxImpactDepth {
		depth = maxImpactDepth
	}

	seen := map[string]bool{}
	seen[fmt.Sprintf("%s:%d", path, line)] = true // don't re-expand the target itself

	var levels [][]impactNode
	frontier := []impactNode{{path: path, line: line, col: col}}
	truncated := false
	total := 0

	for lvl := 0; lvl < depth && len(frontier) > 0; lvl++ {
		var callers []impactNode
		var next []impactNode
		for _, f := range frontier {
			locs, _, err := m.References(ctx, f.path, f.line, f.col)
			if err != nil {
				continue
			}
			for _, loc := range locs {
				if ctx.Err() != nil {
					truncated = true
					break
				}
				rpath := lsp.URIToPath(loc.URI)
				enc, ok, err := m.EnclosingSymbol(ctx, rpath, loc.Range.Start.Line+1, loc.Range.Start.Character+1)
				if err != nil || !ok {
					continue
				}
				// Skip the reference that IS the target's own declaration (it resolves to the
				// target function itself), and any caller already recorded.
				if enc.Path == f.path && enc.DefLine == f.line {
					continue
				}
				key := fmt.Sprintf("%s:%d:%s", enc.Path, enc.DefLine, enc.Name)
				if seen[key] {
					continue
				}
				seen[key] = true
				n := impactNode{key: key, path: enc.Path, line: enc.DefLine, col: enc.DefCol, name: enc.Name, kind: enc.Kind}
				if total >= maxImpactTotal || len(callers) >= maxImpactPerLevel {
					truncated = true
					continue
				}
				callers = append(callers, n)
				next = append(next, n)
				total++
			}
		}
		if len(callers) == 0 {
			break
		}
		levels = append(levels, callers)
		frontier = next
	}

	s.toolLine(true, "CodeNav", "impact", fmt.Sprintf("%d callers", total), false)
	return textResult(formatImpact(s.root, levels, total, truncated))
}

// formatImpact renders the call graph as a compact, level-by-level tree with repo-relative
// paths and a test-coverage tally per level (a change with test callers is safer to make).
func formatImpact(root string, levels [][]impactNode, total int, truncated bool) string {
	if total == 0 {
		return "No callers found — the symbol appears unused (or the language server saw no references). Safe to change from a call-site standpoint, but check reflection/dynamic dispatch and other languages."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Impact / blast radius — %d transitive caller(s):\n", total)
	for i, lvl := range levels {
		// Deterministic order: by path then line.
		sort.Slice(lvl, func(a, c int) bool {
			if lvl[a].path != lvl[c].path {
				return lvl[a].path < lvl[c].path
			}
			return lvl[a].line < lvl[c].line
		})
		tests := 0
		for _, n := range lvl {
			if isTestPath(n.path) {
				tests++
			}
		}
		fmt.Fprintf(&b, "\nLevel %d — %d caller(s)", i+1, len(lvl))
		if tests > 0 {
			fmt.Fprintf(&b, " (%d in tests)", tests)
		}
		b.WriteString(":\n")
		for _, n := range lvl {
			rel := n.path
			if r, err := filepath.Rel(root, n.path); err == nil && !strings.HasPrefix(r, "..") {
				rel = r
			}
			tag := ""
			if isTestPath(n.path) {
				tag = "  [test]"
			}
			fmt.Fprintf(&b, "  %s:%d  %s  <%s>%s\n", rel, n.line, n.name, symbolKindLabel(n.kind), tag)
		}
	}
	if truncated {
		b.WriteString("\n… truncated (a hot symbol with many callers; caps: " +
			fmt.Sprintf("%d/level, %d total", maxImpactPerLevel, maxImpactTotal) +
			"). Narrow with find-references on a specific caller.")
	}
	return strings.TrimRight(b.String(), "\n")
}

// symbolKindLabel maps an LSP SymbolKind to a short human label.
func symbolKindLabel(kind int) string {
	switch kind {
	case 5:
		return "class"
	case 6:
		return "method"
	case 9:
		return "constructor"
	case 11:
		return "interface"
	case 12:
		return "func"
	case 13:
		return "var"
	case 14:
		return "const"
	case 23:
		return "struct"
	default:
		return "symbol"
	}
}
