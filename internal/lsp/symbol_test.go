package lsp

import (
	"encoding/json"
	"testing"
)

func rng(sl, sc, el, ec int) Range {
	return Range{Start: Position{Line: sl, Character: sc}, End: Position{Line: el, Character: ec}}
}

func TestDeepestEnclosingPrefersTightest(t *testing.T) {
	// A type (lines 0..20) containing a method (lines 5..10). A position inside the method
	// must attribute to the method, not the type.
	syms := []DocumentSymbol{{
		Name: "Server", Kind: 23, Range: rng(0, 0, 20, 0), SelectionRange: rng(0, 5, 0, 11),
		Children: []DocumentSymbol{
			{Name: "Serve", Kind: 6, Range: rng(5, 0, 10, 1), SelectionRange: rng(5, 8, 5, 13)},
			{Name: "Close", Kind: 6, Range: rng(12, 0, 15, 1), SelectionRange: rng(12, 8, 12, 13)},
		},
	}}
	sym, ok := deepestEnclosing(syms, Position{Line: 7, Character: 4})
	if !ok || sym.Name != "Serve" {
		t.Fatalf("pos in Serve body → %q ok=%v, want Serve", sym.Name, ok)
	}
	// A position in the type but outside any method attributes to the type.
	sym, ok = deepestEnclosing(syms, Position{Line: 1, Character: 0})
	if !ok || sym.Name != "Server" {
		t.Fatalf("pos in type body → %q, want Server", sym.Name)
	}
	// A position outside everything → no enclosing symbol.
	if _, ok := deepestEnclosing(syms, Position{Line: 30, Character: 0}); ok {
		t.Fatalf("pos outside all symbols should not resolve")
	}
}

func TestRangeContainsBoundaries(t *testing.T) {
	r := rng(2, 4, 6, 10)
	cases := []struct {
		p    Position
		want bool
	}{
		{Position{2, 4}, true},   // start boundary
		{Position{6, 10}, true},  // end boundary
		{Position{4, 0}, true},   // interior line, any col
		{Position{2, 3}, false},  // before start col on start line
		{Position{6, 11}, false}, // after end col on end line
		{Position{1, 99}, false}, // before start line
		{Position{7, 0}, false},  // after end line
	}
	for _, c := range cases {
		if got := rangeContains(r, c.p); got != c.want {
			t.Errorf("rangeContains(%v, %v) = %v, want %v", r, c.p, got, c.want)
		}
	}
}

func TestDecodeSymbolsHierarchicalAndFlat(t *testing.T) {
	hier := `[{"name":"F","kind":12,"range":{"start":{"line":0,"character":0},"end":{"line":3,"character":0}},"selectionRange":{"start":{"line":0,"character":5},"end":{"line":0,"character":6}}}]`
	got := decodeSymbols(json.RawMessage(hier))
	if len(got) != 1 || got[0].Name != "F" || got[0].Range.End.Line != 3 || got[0].SelectionRange.Start.Character != 5 {
		t.Fatalf("hierarchical decode wrong: %+v", got)
	}
	// SymbolInformation[] fallback: flat, carries a `location` instead of selectionRange.
	flat := `[{"name":"G","kind":6,"location":{"uri":"file:///x.go","range":{"start":{"line":9,"character":2},"end":{"line":9,"character":3}}}}]`
	got = decodeSymbols(json.RawMessage(flat))
	if len(got) != 1 || got[0].Name != "G" || got[0].Range.Start.Line != 9 || got[0].SelectionRange.Start.Line != 9 {
		t.Fatalf("flat decode wrong: %+v", got)
	}
	if decodeSymbols(json.RawMessage(`null`)) != nil {
		t.Fatalf("null should decode to nil")
	}
}
