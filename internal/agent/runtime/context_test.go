package runtime

import (
	"go/build"
	"strings"
	"testing"
)

func TestSupplementalBlockEmptyIsNoop(t *testing.T) {
	// The invariant: no supplemental context => empty block => the engine's own
	// (project + user-global) context is unchanged, byte for byte.
	if got := supplementalBlock(nil); got != "" {
		t.Errorf("nil context should yield no block, got %q", got)
	}
	if got := supplementalBlock([]ContextItem{{Kind: KindMemory, Content: "   "}}); got != "" {
		t.Errorf("all-blank content should yield no block, got %q", got)
	}
}

func TestSupplementalBlockDeterministicOrder(t *testing.T) {
	// Given out of order, the engine renders by fixed Kind precedence
	// (instruction < memory < reference < history), independent of input order.
	items := []ContextItem{
		{Kind: KindHistory, Content: "h"},
		{Kind: KindInstruction, Content: "i"},
		{Kind: KindReference, Content: "r"},
		{Kind: KindMemory, Content: "m"},
	}
	block := supplementalBlock(items)
	order := []string{}
	for _, line := range strings.Split(block, "\n") {
		switch strings.TrimSpace(line) {
		case "i", "m", "r", "h":
			order = append(order, strings.TrimSpace(line))
		}
	}
	if got := strings.Join(order, ""); got != "imrh" {
		t.Errorf("content order = %q, want imrh (fixed Kind precedence)", got)
	}
}

// TestEngineDoesNotImportGateway enforces the dependency direction: the coding
// engine is a stable capability that knows nothing of the orchestration above it.
// It must not import gateway or channel packages.
func TestEngineDoesNotImportGateway(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if strings.Contains(imp, "internal/gateway") || strings.Contains(imp, "internal/channels") {
			t.Errorf("coding engine must not import the agent/gateway layer, but imports %q", imp)
		}
	}
}
