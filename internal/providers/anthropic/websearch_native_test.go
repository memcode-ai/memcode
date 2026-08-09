package anthropic

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

func searchToolReq() wire.Request {
	return wire.Request{
		Model: "m",
		Tools: []wire.ToolDef{
			{Name: "read_file", InputSchema: map[string]any{"type": "object"}},
			{Name: "web_search", InputSchema: map[string]any{"type": "object"}},
			{Name: "bash", InputSchema: map[string]any{"type": "object"}},
		},
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}}},
	}
}

func TestAnthropicNativeWebSearch(t *testing.T) {
	w := buildWire(searchToolReq(), 4096, false)
	if !w.NativeWebSearch {
		t.Fatal("buildWire must flag the stripped web_search def")
	}
	if len(w.Tools) != 2 || w.Tools[0].Name != "read_file" || w.Tools[1].Name != "bash" {
		t.Fatalf("web_search def must be stripped from wire tools: %#v", w.Tools)
	}
	// The 1h cache breakpoint stays on the last REAL function tool.
	if w.Tools[0].CacheControl != nil || w.Tools[1].CacheControl == nil {
		t.Fatalf("cache breakpoint must sit on the last function tool: %#v", w.Tools)
	}
	p := wireToParams(w)
	if n := len(p.Tools); n != 3 {
		t.Fatalf("params must carry 2 function tools + the native search tool, got %d", n)
	}
	last := p.Tools[len(p.Tools)-1]
	if last.OfWebSearchTool20250305 == nil {
		t.Fatalf("native web_search_20250305 must be appended last: %#v", last)
	}
	for _, tu := range p.Tools[:len(p.Tools)-1] {
		if tu.OfTool == nil || tu.OfTool.Name == "web_search" {
			t.Fatalf("function tools must survive without the web_search def: %#v", tu)
		}
	}
	// No def → no native tool, no flag.
	w = buildWire(wire.Request{Tools: []wire.ToolDef{{Name: "bash", InputSchema: map[string]any{"type": "object"}}}}, 4096, false)
	if w.NativeWebSearch || len(wireToParams(w).Tools) != 1 {
		t.Fatal("native search must only appear when the def was declared")
	}
}
