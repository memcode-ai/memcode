package provider

import (
	"context"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/wire"
)

// The PDF-native contract: document blocks reach every capable vendor's native
// input (Anthropic document, OpenAI input_file, Gemini inline blob) in BOTH
// positions (top-level user block and inside a tool_result), and the router
// absorbs document turns away from models without native PDF input.

var pdfBytes = []byte("%PDF-1.4 fake")

func docBlock() wire.Block { return wire.DocumentBlock("application/pdf", pdfBytes) }

func toolResultWithDoc(id string) wire.Block {
	return wire.ToolResultBlocks(id, []wire.Block{
		wire.TextBlock("read 3 pages"),
		docBlock(),
	}, false)
}

// Router: a document turn on a model without native PDF input is a TYPED
// capability error — the CLI pre-checks from the shared catalog and picks a
// capable model; the gateway never retargets. A PDF-capable model serves the
// exact requested id.
func TestDocumentTurnTypedContract(t *testing.T) {
	h := servingHybrid("http://unused.invalid")
	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{docBlock()}}}

	// Cheap lane (no pdf) → typed document capability error.
	_, err := h.Complete(context.Background(), wire.Request{
		Model: "accounts/fireworks/models/glm-5p2", Messages: msgs,
	})
	if ce := AsCapabilityError(err); ce == nil || ce.Capability != "document" {
		t.Fatalf("cheap-lane document turn err = %v, want CapabilityError{document}", err)
	}

	// grok-4.5: vision-capable but no native PDF input → the same typed error.
	_, err = h.Complete(context.Background(), wire.Request{
		Model: catalog.ModelGrok45, Messages: msgs,
	})
	if ce := AsCapabilityError(err); ce == nil || ce.Capability != "document" {
		t.Fatalf("grok document turn err = %v, want CapabilityError{document}", err)
	}

	// sonnet (two-vendor router so the model has an owner): native PDF input →
	// serves the exact requested id, no fallback.
	h2 := servingHybrid("http://unused.invalid")
	h2.strong["anthropic"] = StrongTier{Vendor: "anthropic", Provider: fakeStrong{}}
	resp, err := h2.Complete(context.Background(), wire.Request{
		Model: catalog.ModelSonnet, Messages: msgs,
	})
	if err != nil {
		t.Fatalf("Complete (sonnet): %v", err)
	}
	if resp.FallbackReason != "" || resp.Model != catalog.ModelSonnet {
		t.Fatalf("sonnet document turn = model %q fallback %q, want sonnet / none", resp.Model, resp.FallbackReason)
	}
}

// Catalog sanity: the pdf capability matches what the adapters can actually do.
func TestPDFCapabilityFlags(t *testing.T) {
	for label, want := range map[string]bool{
		"sonnet": true, "opus": true, "haiku": true,
		"sol": true, "terra": true, "luna": true,
		"gemini-pro": true, "gemini-flash": true, "gemini-flash-lite": true,
		"grok-4.5": false, "glm-5p2": false, "kimi-k2p6": false,
	} {
		spec, ok := reg.specByLabel(label)
		if !ok || spec.PDF != want {
			t.Errorf("catalog %s: pdf = %v (found %v), want %v", label, spec.PDF, ok, want)
		}
	}
}
