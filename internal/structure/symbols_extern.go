package structure

// Non-Go symbol extraction for the repo map. TypeScript/JavaScript/Python
// declarations come from the LANGUAGE SERVER's documentSymbol (the same
// detect-and-connect LSP layer code_nav/diagnostics use) — a real parser per
// language without a tree-sitter/CGO dependency; if the server isn't on PATH
// the language degrades to absent and the digest says what to install.
//
// The lister is injected as a function (ExternLister) so this package stays
// decoupled from internal/lsp; the runtime wires it to the session's resident
// Manager.
//
// References are LEXICAL for these languages: identifier occurrences matched
// against the language group's symbol names, attributed to the enclosing
// declaration by documentSymbol line ranges, weight split across same-name
// symbols. Cross-language edges are deliberately absent (a TS identifier never
// references a Go func).
//
// documentSymbol over a whole repo is slow (tens of ms per file), so per-file
// results are cached in the store keyed by content hash: only changed files
// re-query, and a per-call time budget keeps the first (cold) build from
// stalling a turn — coverage completes incrementally across calls.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/store"
)

// ExternSymbol is one declaration reported by an external (LSP) lister.
type ExternSymbol struct {
	Name    string `json:"name"`
	Kind    int    `json:"kind"` // LSP SymbolKind
	Line    int    `json:"line"`
	EndLine int    `json:"end_line"`
	SelLine int    `json:"sel_line"`
	Depth   int    `json:"depth"`
}

// ExternLister returns a file's declarations, or ok=false when no server
// serves the file's language.
type ExternLister func(ctx context.Context, absPath string) ([]ExternSymbol, bool)

const (
	externCacheScope = "repo"
	externCacheLayer = "symbol_cache"
	// externTimeBudget bounds COLD documentSymbol queries per call; cached files
	// are free. Coverage completes incrementally across calls.
	externTimeBudget = 3 * time.Second
)

// lspKindLabel maps the LSP SymbolKind ints the map cares about.
var lspKindLabel = map[int]string{
	5: "class", 6: "method", 9: "ctor", 10: "enum", 11: "interface",
	12: "func", 13: "var", 14: "const", 23: "struct",
}

// externLangFor groups files into reference-matching ecosystems.
func externLangFor(rel string) string {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return "ts"
	case ".py", ".pyi":
		return "py"
	}
	return ""
}

func externTestFile(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py")
}

// cachedFileSyms is the persisted per-file extraction result.
type cachedFileSyms struct {
	Hash string         `json:"hash"`
	Syms []ExternSymbol `json:"syms"`
}

type externCache map[string]cachedFileSyms // rel path → result

func loadExternCache(ctx context.Context, st store.Store) externCache {
	c := externCache{}
	if st == nil {
		return c
	}
	state, ok, err := st.GetState(ctx, externCacheScope, externCacheLayer)
	if err != nil || !ok || len(state.Body) == 0 {
		return c
	}
	_ = json.Unmarshal(state.Body, &c)
	return c
}

func saveExternCache(ctx context.Context, st store.Store, c externCache) {
	if st == nil {
		return
	}
	body, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = st.PutState(ctx, store.State{
		Scope: externCacheScope, Layer: externCacheLayer,
		Body: body, RefreshedAt: time.Now().UTC(),
	})
}

// addExternSymbols extracts TS/JS/Python symbols + references into g.
// Returns which language groups had files but no server (for the digest note)
// and how many cold files the time budget deferred to later calls.
func addExternSymbols(ctx context.Context, st store.Store, root string, files []string, lister ExternLister, g *SymbolGraph) (missing []string, deferred int) {
	if lister == nil {
		lister = func(context.Context, string) ([]ExternSymbol, bool) { return nil, false }
	}
	cache := loadExternCache(ctx, st)
	next := externCache{}
	missingSet := map[string]bool{}
	changed := false
	deadline := time.Now().Add(externTimeBudget)

	type fileText struct {
		rel, lang string
		lines     []string
		symStart  int // index of this file's first symbol in g.Symbols
		symCount  int
	}
	var texts []fileText

	for _, rel := range files {
		lang := externLangFor(rel)
		if lang == "" || externTestFile(rel) || ctx.Err() != nil {
			continue
		}
		abs := filepath.Join(root, rel)
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:8])

		var syms []ExternSymbol
		if hit, ok := cache[rel]; ok && hit.Hash == hash {
			syms = hit.Syms
		} else {
			if time.Now().After(deadline) {
				deferred++
				continue // budget spent — this file joins the map on a later call
			}
			var ok bool
			syms, ok = lister(ctx, abs)
			if !ok {
				missingSet[lang] = true
				continue
			}
			syms = filterExtern(syms)
			changed = true
		}
		next[rel] = cachedFileSyms{Hash: hash, Syms: syms}
		if len(syms) == 0 {
			continue
		}
		lines := strings.Split(string(data), "\n")
		ft := fileText{rel: rel, lang: lang, lines: lines, symStart: len(g.Symbols), symCount: len(syms)}
		for _, es := range syms {
			decl := ""
			if es.Line-1 >= 0 && es.Line-1 < len(lines) {
				decl = strings.TrimSpace(lines[es.Line-1])
			}
			kind := lspKindLabel[es.Kind]
			if kind == "" {
				kind = "sym"
			}
			g.Symbols = append(g.Symbols, Symbol{
				Name: es.Name, Kind: kind, Lang: lang, Path: rel,
				Line: es.Line, EndLine: es.EndLine, Decl: decl,
			})
		}
		texts = append(texts, ft)
	}
	if changed || len(next) != len(cache) {
		saveExternCache(ctx, st, next)
	}
	for len(g.InDeg) < len(g.Symbols) {
		g.InDeg = append(g.InDeg, 0)
	}

	// Name index per language group (≥3 chars, same rule as Go).
	byLang := map[string]map[string][]int{}
	for i, s := range g.Symbols {
		if s.Lang == "go" || len(s.Name) < 3 {
			continue
		}
		if byLang[s.Lang] == nil {
			byLang[s.Lang] = map[string][]int{}
		}
		byLang[s.Lang][s.Name] = append(byLang[s.Lang][s.Name], i)
	}

	// Lexical reference pass: identifier occurrences → same-language symbols,
	// attributed to the tightest enclosing declaration range in the source file.
	for _, ft := range texts {
		names := byLang[ft.lang]
		if len(names) == 0 {
			continue
		}
		fileSyms := make([]int, 0, ft.symCount)
		for i := ft.symStart; i < ft.symStart+ft.symCount; i++ {
			fileSyms = append(fileSyms, i)
		}
		encloser := func(line int) int {
			best, bestSpan := -1, 1<<30
			for _, si := range fileSyms {
				s := g.Symbols[si]
				if s.Line <= line && line <= s.EndLine {
					if span := s.EndLine - s.Line; span < bestSpan {
						best, bestSpan = si, span
					}
				}
			}
			return best
		}
		for lineNo, line := range ft.lines {
			for _, word := range identWords(line) {
				targets := names[word]
				if len(targets) == 0 {
					continue
				}
				from := encloser(lineNo + 1)
				w := 1.0 / float64(len(targets))
				for _, to := range targets {
					// The declaration line itself is not a reference.
					if from == to || (g.Symbols[to].Path == ft.rel && g.Symbols[to].Line == lineNo+1) {
						continue
					}
					g.InDeg[to] += w
					if from < 0 {
						continue
					}
					m := g.Refs[from]
					if m == nil {
						m = map[int]float64{}
						g.Refs[from] = m
					}
					m[to] += w
				}
			}
		}
	}

	for lang := range missingSet {
		missing = append(missing, lang)
	}
	sort.Strings(missing)
	return missing, deferred
}

// filterExtern keeps declaration kinds worth mapping: containers and callables
// at the top level, plus member methods/ctors. Depth-1 variables/fields are
// locals-and-state noise.
func filterExtern(syms []ExternSymbol) []ExternSymbol {
	out := make([]ExternSymbol, 0, len(syms))
	for _, s := range syms {
		if lspKindLabel[s.Kind] == "" || len(s.Name) < 2 {
			continue
		}
		if s.Depth > 0 && s.Kind != 6 && s.Kind != 9 && s.Kind != 12 {
			continue
		}
		out = append(out, s)
	}
	return out
}

// identWords yields identifier-shaped tokens (letters/digits/_/$, len ≥ 3) in a
// line. Lexical matching, same as the search tools — not structure parsing.
func identWords(line string) []string {
	var words []string
	start := -1
	isIdent := func(r byte) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '$'
	}
	for i := 0; i <= len(line); i++ {
		if i < len(line) && isIdent(line[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			if i-start >= 3 {
				words = append(words, line[start:i])
			}
			start = -1
		}
	}
	return words
}
