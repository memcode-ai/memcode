package provcore

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

func TestSplitWebSearchTool(t *testing.T) {
	tools := []wire.ToolDef{
		{Name: "read_file", InputSchema: map[string]any{"type": "object"}},
		{Name: "web_search", InputSchema: map[string]any{"type": "object"}},
		{Name: "bash", InputSchema: map[string]any{"type": "object"}},
	}
	rest, had := SplitWebSearchTool(tools)
	if !had || len(rest) != 2 || rest[0].Name != "read_file" || rest[1].Name != "bash" {
		t.Fatalf("split wrong: had=%v rest=%#v", had, rest)
	}
	// The caller's slice is never mutated.
	if len(tools) != 3 || tools[1].Name != "web_search" {
		t.Fatalf("input mutated: %#v", tools)
	}
	// Absent def → untouched, false.
	rest, had = SplitWebSearchTool(tools[:1])
	if had || len(rest) != 1 {
		t.Fatalf("no-def case wrong: had=%v rest=%#v", had, rest)
	}
}
