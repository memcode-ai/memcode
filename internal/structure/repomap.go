package structure

// The repo map: a token-budgeted, ranked digest of the repo's important
// symbols and where they live — the repo-wide retrieval primitive that
// complements code_query (find ONE thing) and code_nav impact (one symbol's
// blast radius). Aider-style: extract symbols + name-level references
// (symbols.go), rank with personalized PageRank (pagerank.go), render the top
// files with their top declaration lines under a token budget.
//
// Deterministic and rebuilt per call from the working tree (extraction on this
// repo — ~1600 files — is sub-second; always-fresh beats a HEAD-keyed cache
// that goes stale on every uncommitted edit). Personalization seeds: the
// caller's focus terms (paths or symbol names) plus git-dirty files, so the
// map centers on what's being worked on.

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/repofiles"
	"github.com/memcode-ai/memcode/internal/store"
)

const (
	mapDefaultBudget = 1200 // tokens (~4 chars each); fits well inside the 8KB tool cap
	mapMaxBudget     = 1800
	mapMaxFiles      = 30
	mapMaxPerFile    = 8
	mapDeclClip      = 110 // clip declaration lines — signatures, not bodies
)

// MapOptions steers one repo-map build.
type MapOptions struct {
	Focus  []string     // paths or symbol names to center the map on (personalization seeds)
	Budget int          // token budget for the digest (0 = default)
	Store  store.Store  // optional: persists the extern (LSP) symbol cache
	Extern ExternLister // optional: TS/JS/Python declarations (LSP documentSymbol)
}

// BuildSymbolMap builds the ranked symbol digest for the repo at root: Go via
// AST, TS/JS/Python via the injected LSP lister (absent language servers
// degrade to a note, never an error).
func BuildSymbolMap(ctx context.Context, root string, opts MapOptions) (string, error) {
	files := repofiles.List(ctx, root)
	g := ExtractGoSymbols(ctx, root, files)
	missing, deferred := addExternSymbols(ctx, opts.Store, root, files, opts.Extern, &g)
	if len(g.Symbols) == 0 {
		return "no symbols found (Go is parsed natively; TS/JS/Python need their language server on PATH — use code_query/ripgrep here)", nil
	}

	seeds := mapSeeds(g, opts.Focus, gitDirtyFiles(ctx, root))
	ranks := PageRank(len(g.Symbols), g.Refs, seeds)

	budget := opts.Budget
	if budget <= 0 {
		budget = mapDefaultBudget
	}
	if budget > mapMaxBudget {
		budget = mapMaxBudget
	}
	return renderMap(g, ranks, opts.Focus, budget, missing, deferred), nil
}

// mapSeeds builds the personalization vector: a symbol seeds when its file
// matches a focus path prefix or a dirty file, or its name matches a focus
// term. Focus terms outweigh dirty files (an explicit ask beats ambient state).
func mapSeeds(g SymbolGraph, focus []string, dirty []string) map[int]float64 {
	seeds := map[int]float64{}
	dirtySet := map[string]bool{}
	for _, d := range dirty {
		dirtySet[d] = true
	}
	var focusPaths, focusNames []string
	for _, f := range focus {
		f = strings.TrimSpace(strings.TrimSuffix(f, "/"))
		if f == "" {
			continue
		}
		if strings.ContainsAny(f, "/.") {
			focusPaths = append(focusPaths, f)
		} else {
			focusNames = append(focusNames, strings.ToLower(f))
		}
	}
	// Weights are EXTRA teleport mass on top of the uniform base (see pageRank):
	// an explicit focus dominates decisively, dirty files nudge.
	for i, s := range g.Symbols {
		w := 0.0
		for _, fp := range focusPaths {
			if s.Path == fp || strings.HasPrefix(s.Path, fp+"/") {
				w += 12
			}
		}
		lower := strings.ToLower(s.Name)
		for _, fn := range focusNames {
			if lower == fn {
				w += 20
			} else if strings.Contains(lower, fn) {
				w += 8
			}
		}
		if dirtySet[s.Path] {
			w += 3
		}
		if w > 0 {
			seeds[i] = w
		}
	}
	return seeds
}

// renderMap renders the digest: files ordered by their symbols' aggregate rank,
// each showing its top declaration lines with line numbers and in-degree.
func renderMap(g SymbolGraph, ranks []float64, focus []string, budget int, missing []string, deferred int) string {
	type fileAgg struct {
		path  string
		score float64
		syms  []int
	}
	byFile := map[string]*fileAgg{}
	for i := range g.Symbols {
		f := byFile[g.Symbols[i].Path]
		if f == nil {
			f = &fileAgg{path: g.Symbols[i].Path}
			byFile[g.Symbols[i].Path] = f
		}
		f.score += ranks[i]
		f.syms = append(f.syms, i)
	}
	files := make([]*fileAgg, 0, len(byFile))
	for _, f := range byFile {
		sort.Slice(f.syms, func(a, b int) bool { return ranks[f.syms[a]] > ranks[f.syms[b]] })
		files = append(files, f)
	}
	sort.Slice(files, func(a, b int) bool {
		if files[a].score != files[b].score {
			return files[a].score > files[b].score
		}
		return files[a].path < files[b].path
	})

	var refCount int
	for _, tos := range g.Refs {
		refCount += len(tos)
	}
	langCount := map[string]int{}
	for _, s := range g.Symbols {
		langCount[s.Lang]++
	}
	var langs []string
	for _, l := range []string{"go", "ts", "py"} {
		if langCount[l] > 0 {
			langs = append(langs, fmt.Sprintf("%s %d", l, langCount[l]))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Repo map — %d symbols (%s), %d reference edges, ranked by connectivity",
		len(g.Symbols), strings.Join(langs, ", "), refCount)
	if len(focus) > 0 {
		fmt.Fprintf(&b, ", focused on: %s", strings.Join(focus, " "))
	}
	b.WriteString("\n")
	for _, m := range missing {
		server := map[string]string{"ts": "typescript-language-server", "py": "pyright-langserver"}[m]
		fmt.Fprintf(&b, "note: %s files present but not mapped — install %s to include them\n", m, server)
	}
	if deferred > 0 {
		fmt.Fprintf(&b, "note: %d file(s) still indexing (time-budgeted) — call again to complete coverage\n", deferred)
	}

	charBudget := budget * 4 // the repo-wide ~4 chars/token heuristic
	shown := 0
	for _, f := range files {
		if shown >= mapMaxFiles || b.Len() >= charBudget {
			break
		}
		var sec strings.Builder
		fmt.Fprintf(&sec, "\n%s\n", f.path)
		wrote := 0
		for _, si := range f.syms {
			if wrote >= mapMaxPerFile {
				break
			}
			s := g.Symbols[si]
			decl := s.Decl
			if len(decl) > mapDeclClip {
				decl = decl[:mapDeclClip] + "…"
			}
			in := int(g.InDeg[si] + 0.5)
			suffix := ""
			if in > 0 {
				suffix = fmt.Sprintf("  ·%d refs", in)
			}
			fmt.Fprintf(&sec, "  %d: %s%s\n", s.Line, decl, suffix)
			wrote++
		}
		if wrote == 0 {
			continue
		}
		if b.Len()+sec.Len() > charBudget && shown > 0 {
			break
		}
		b.WriteString(sec.String())
		shown++
	}
	if shown < len(files) {
		fmt.Fprintf(&b, "\n(+%d more files below the budget — raise budget_tokens or pass focus to zoom)\n", len(files)-shown)
	}
	return b.String()
}

// gitDirtyFiles lists working-tree-modified paths relative to root (the
// ambient personalization seed). git prints paths relative to the GIT root,
// which in a monorepo is above the scan root — strip the prefix and drop
// entries outside it. Nil-safe on non-git roots or git errors.
func gitDirtyFiles(ctx context.Context, root string) []string {
	prefix := ""
	if out, err := gitIn(ctx, root, "rev-parse", "--show-prefix"); err == nil {
		prefix = strings.TrimSpace(out)
	}
	out, err := gitIn(ctx, root, "status", "--porcelain")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 { // rename: old -> new
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`)
		if prefix != "" {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			path = path[len(prefix):]
		}
		if path != "" {
			files = append(files, path)
		}
	}
	return files
}

func gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
