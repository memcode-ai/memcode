package structure

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/memcode-ai/memcode/internal/store"
)

// fakeTSLister emulates an LSP documentSymbol response for the test tree's
// app.ts / lib.ts / util.py without needing a real server.
func fakeTSLister(calls *int64) ExternLister {
	return func(_ context.Context, abs string) ([]ExternSymbol, bool) {
		atomic.AddInt64(calls, 1)
		switch filepath.Base(abs) {
		case "lib.ts":
			return []ExternSymbol{
				{Name: "renderMap", Kind: 12, Line: 1, EndLine: 3, SelLine: 1},
				{Name: "Renderer", Kind: 5, Line: 5, EndLine: 9, SelLine: 5},
				{Name: "draw", Kind: 6, Line: 6, EndLine: 8, SelLine: 6, Depth: 1},
			}, true
		case "app.ts":
			return []ExternSymbol{
				{Name: "main", Kind: 12, Line: 1, EndLine: 4, SelLine: 1},
			}, true
		case "util.py":
			return nil, false // no pyright on PATH
		}
		return nil, true
	}
}

func writeExternTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/m\n",
		"lib.ts": `export function renderMap(x: number) {
  return x
}

export class Renderer {
  draw() {
    return renderMap(1)
  }
}
`,
		"app.ts": `import { renderMap, Renderer } from "./lib"
function main() {
  return new Renderer().draw() + renderMap(2)
}
`,
		"util.py":     "def helper():\n    return 1\n",
		"app.test.ts": "renderMap(99) // test files must be skipped\n",
	}
	for rel, src := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestExternSymbolsAndLexicalRefs(t *testing.T) {
	root := writeExternTree(t)
	var calls int64
	g := ExtractGoSymbols(context.Background(), root, nil)
	missing, deferred := addExternSymbols(context.Background(), nil, root,
		[]string{"lib.ts", "app.ts", "util.py", "app.test.ts"}, fakeTSLister(&calls), &g)
	if deferred != 0 {
		t.Errorf("nothing should be deferred, got %d", deferred)
	}
	if len(missing) != 1 || missing[0] != "py" {
		t.Errorf("expected py reported missing, got %v", missing)
	}
	find := func(path, name string) int {
		for i, s := range g.Symbols {
			if s.Path == path && s.Name == name {
				return i
			}
		}
		t.Fatalf("symbol %s:%s not found in %v", path, name, g.Symbols)
		return -1
	}
	rm := find("lib.ts", "renderMap")
	if g.Symbols[rm].Lang != "ts" || g.Symbols[rm].Kind != "func" {
		t.Errorf("renderMap symbol wrong: %+v", g.Symbols[rm])
	}
	// renderMap referenced from: Renderer.draw (lib.ts line 7), app.ts import
	// line + main body — its own declaration line must NOT count.
	if g.InDeg[rm] < 2 {
		t.Errorf("renderMap should have ≥2 incoming refs, got %.1f", g.InDeg[rm])
	}
	// The edge from draw → renderMap must attribute to the METHOD (tightest range).
	draw := find("lib.ts", "draw")
	if g.Refs[draw][rm] <= 0 {
		t.Errorf("draw should reference renderMap, refs: %v", g.Refs[draw])
	}
	// Test file skipped entirely.
	for _, s := range g.Symbols {
		if strings.Contains(s.Path, ".test.") {
			t.Errorf("test file leaked into symbols: %+v", s)
		}
	}
}

func TestExternCacheAvoidsRelisting(t *testing.T) {
	root := writeExternTree(t)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	files := []string{"lib.ts", "app.ts"}

	var calls int64
	g1 := SymbolGraph{Refs: map[int]map[int]float64{}}
	addExternSymbols(context.Background(), st, root, files, fakeTSLister(&calls), &g1)
	first := atomic.LoadInt64(&calls)
	if first != 2 {
		t.Fatalf("expected 2 lister calls on cold build, got %d", first)
	}

	g2 := SymbolGraph{Refs: map[int]map[int]float64{}}
	addExternSymbols(context.Background(), st, root, files, fakeTSLister(&calls), &g2)
	if atomic.LoadInt64(&calls) != first {
		t.Errorf("warm build must not re-query the lister (calls %d → %d)", first, calls)
	}
	if len(g2.Symbols) != len(g1.Symbols) {
		t.Errorf("cached build lost symbols: %d vs %d", len(g2.Symbols), len(g1.Symbols))
	}

	// Invalidation: change a file → exactly that file re-queries.
	if err := os.WriteFile(filepath.Join(root, "app.ts"), []byte("function main() { return 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g3 := SymbolGraph{Refs: map[int]map[int]float64{}}
	addExternSymbols(context.Background(), st, root, files, fakeTSLister(&calls), &g3)
	if got := atomic.LoadInt64(&calls); got != first+1 {
		t.Errorf("changed file should re-query once: calls %d → %d", first, got)
	}
}

func TestBuildSymbolMapMixedLanguages(t *testing.T) {
	root := writeExternTree(t)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc GoHub() int { return 1 }\nfunc caller() int { return GoHub() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls int64
	digest, err := BuildSymbolMap(context.Background(), root, MapOptions{Extern: fakeTSLister(&calls)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(digest, "go 2") || !strings.Contains(digest, "ts 4") {
		t.Errorf("header should count both languages:\n%s", firstLineOf(digest))
	}
	if !strings.Contains(digest, "lib.ts") {
		t.Errorf("TS files should render:\n%s", digest)
	}
	if !strings.Contains(digest, "install pyright-langserver") {
		t.Errorf("missing-server note absent:\n%s", digest)
	}
}
