package structure

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTree lays down a tiny Go module: a hub symbol referenced across
// packages via import selectors, a same-package bare reference, a shadowing
// local Dispatch that must NOT bleed cross-package, and a test file that must
// be ignored.
func writeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"core/engine.go": `package core

// Dispatch is the hub everything calls.
func Dispatch(name string) string { return name }

type Engine struct{}

func (e *Engine) Run() string { return Dispatch("run") }
`,
		"api/handler.go": `package api

import "example.com/m/core"

func Handle() string { return core.Dispatch("h") + core.Dispatch("h2") }
`,
		"cmd/main.go": `package main

import "example.com/m/core"

func main() { _ = core.Dispatch("boot"); _ = Dispatch("local"); helperLeaf() }
func Dispatch(x string) string { return x } // same-name local: bare refs stay in-package
func helperLeaf() {}
`,
		"core/engine_test.go": `package core

func TestNothing() { Dispatch("ignored-in-tests") }
`,
	}
	for rel, src := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestExtractGoSymbols(t *testing.T) {
	root := writeTree(t)
	g := ExtractGoSymbols(context.Background(), root, []string{
		"core/engine.go", "api/handler.go", "cmd/main.go", "core/engine_test.go",
	})
	names := map[string]Symbol{}
	for _, s := range g.Symbols {
		names[s.Path+":"+s.Name] = s
	}
	if s, ok := names["core/engine.go:Dispatch"]; !ok || s.Kind != "func" || !s.Exported {
		t.Errorf("core Dispatch missing or wrong: %+v", s)
	}
	if s, ok := names["core/engine.go:Run"]; !ok || s.Kind != "method" || s.Recv != "Engine" {
		t.Errorf("Run method missing or wrong receiver: %+v", s)
	}
	if s, ok := names["core/engine.go:Engine"]; !ok || s.Kind != "type" {
		t.Errorf("Engine type missing: %+v", s)
	}
	for key := range names {
		if strings.Contains(key, "_test.go") {
			t.Errorf("test file symbols must be skipped: %s", key)
		}
	}
	// Exact resolution: core.Dispatch is referenced via import selectors from
	// api (2×) + cmd (1×) and bare from core.Run (1×) = 4. cmd's same-name local
	// Dispatch gets ONLY its bare in-package ref — no cross-package bleed.
	if len(g.Refs) == 0 {
		t.Fatal("no reference edges extracted")
	}
	inDeg := func(path, name string) float64 {
		for i, s := range g.Symbols {
			if s.Path == path && s.Name == name {
				return g.InDeg[i]
			}
		}
		t.Fatalf("symbol %s:%s not found", path, name)
		return 0
	}
	if got := inDeg("core/engine.go", "Dispatch"); got != 4 {
		t.Errorf("core Dispatch in-degree: want 4, got %.1f", got)
	}
	if got := inDeg("cmd/main.go", "Dispatch"); got != 1 {
		t.Errorf("cmd Dispatch in-degree: want 1 (no cross-package bleed), got %.1f", got)
	}
}

func TestPageRankHubOutranksLeaf(t *testing.T) {
	root := writeTree(t)
	g := ExtractGoSymbols(context.Background(), root, []string{
		"core/engine.go", "api/handler.go", "cmd/main.go",
	})
	ranks := PageRank(len(g.Symbols), g.Refs, nil)
	get := func(path, name string) float64 {
		for i, s := range g.Symbols {
			if s.Path == path && s.Name == name {
				return ranks[i]
			}
		}
		t.Fatalf("symbol %s:%s not found", path, name)
		return 0
	}
	hub := get("core/engine.go", "Dispatch")
	leaf := get("cmd/main.go", "helperLeaf")
	if hub <= leaf {
		t.Errorf("hub Dispatch (%.6f) must outrank leaf helperLeaf (%.6f)", hub, leaf)
	}
}

func TestPageRankPersonalizationShiftsRank(t *testing.T) {
	root := writeTree(t)
	g := ExtractGoSymbols(context.Background(), root, []string{
		"core/engine.go", "api/handler.go", "cmd/main.go",
	})
	plain := PageRank(len(g.Symbols), g.Refs, nil)
	seeds := mapSeeds(g, []string{"cmd/main.go"}, nil)
	if len(seeds) == 0 {
		t.Fatal("focus on cmd/main.go should seed its symbols")
	}
	focused := PageRank(len(g.Symbols), g.Refs, seeds)
	var idx int = -1
	for i, s := range g.Symbols {
		if s.Path == "cmd/main.go" && s.Name == "helperLeaf" {
			idx = i
		}
	}
	if focused[idx] <= plain[idx] {
		t.Errorf("personalization on cmd/ should lift its symbols: plain=%.6f focused=%.6f",
			plain[idx], focused[idx])
	}
}

func TestBuildSymbolMapBudgetAndShape(t *testing.T) {
	root := writeTree(t)
	digest, err := BuildSymbolMap(context.Background(), root, MapOptions{Budget: 300})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(digest, "Repo map —") {
		t.Errorf("header missing: %q", firstLineOf(digest))
	}
	if !strings.Contains(digest, "core/engine.go") {
		t.Errorf("hub file missing from digest:\n%s", digest)
	}
	if !strings.Contains(digest, "func Dispatch(") {
		t.Errorf("declaration lines should render:\n%s", digest)
	}
	if got := len(digest); got > 300*4+600 { // budget + header/footer slack
		t.Errorf("digest blew the budget: %d chars", got)
	}
}

func TestBuildSymbolMapNoGo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := BuildSymbolMap(context.Background(), dir, MapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(digest, "no symbols found") {
		t.Errorf("expected the no-symbols note, got: %q", digest)
	}
}

// TestBuildSymbolMapOnThisModule is the live battery: run over the actual
// memcode CLI module and check the map surfaces genuinely central symbols —
// the "where does X live" orientation code_query can't give in one call.
// Also enforces the design's performance envelope (no cache = must be fast).
func TestBuildSymbolMapOnThisModule(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("module root not found")
	}
	start := time.Now()
	digest, err := BuildSymbolMap(context.Background(), root, MapOptions{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("built in %s; digest %d chars:\n%s", elapsed, len(digest), digest)
	if elapsed > 5*time.Second {
		t.Errorf("map build too slow for per-call recompute: %s", elapsed)
	}
	// Orientation claim: at least some genuinely central subsystems surface in
	// the default map (the exact top-N ordering is rank-dependent; the test pins
	// presence, not order).
	central := 0
	for _, marker := range []string{"internal/store/", "internal/events/", "internal/llm/", "internal/agent/", "internal/config/", "internal/vxui/", "internal/provider/"} {
		if strings.Contains(digest, marker) {
			central++
		}
	}
	if central < 2 {
		t.Errorf("expected ≥2 central subsystems in the default map, got %d", central)
	}
	// Focus must zoom: a runtime-focused map surfaces the agent runtime.
	focused, err := BuildSymbolMap(context.Background(), root, MapOptions{Focus: []string{"internal/agent/runtime"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(focused, "internal/agent/runtime/") {
		t.Errorf("focused map should surface the focus dir:\n%s", focused)
	}
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
