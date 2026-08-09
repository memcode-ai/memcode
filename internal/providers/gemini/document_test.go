package gemini

// Document-input mapping tests relocated with the adapter extraction.

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

var pdfBytes = []byte("%PDF-1.4 fake")

func docBlock() wire.Block { return wire.DocumentBlock("application/pdf", pdfBytes) }

func toolResultWithDoc(id string) wire.Block {
	return wire.ToolResultBlocks(id, []wire.Block{
		wire.TextBlock("read 3 pages"),
		docBlock(),
	}, false)
}

func TestGeminiDocumentBecomesInlineBlob(t *testing.T) {
	g := &Gemini{}
	// Top-level document → inline blob part with the pdf mime.
	p := g.blockToPart(docBlock(), nil)
	if p == nil || p.InlineData == nil || p.InlineData.MIMEType != "application/pdf" || string(p.InlineData.Data) != string(pdfBytes) {
		t.Fatalf("document block did not become an application/pdf inline blob: %+v", p)
	}
	// Inside a tool_result → a sibling inline-data part on the same content.
	contents := g.buildContents(wire.Request{Messages: []wire.Message{
		{Role: "user", Blocks: []wire.Block{toolResultWithDoc("tu_3")}},
	}})
	if len(contents) != 1 {
		t.Fatalf("contents = %d, want 1", len(contents))
	}
	var haveResp, haveBlob bool
	for _, part := range contents[0].Parts {
		if part.FunctionResponse != nil {
			haveResp = true
		}
		if part.InlineData != nil && part.InlineData.MIMEType == "application/pdf" {
			haveBlob = true
		}
	}
	if !haveResp || !haveBlob {
		t.Fatalf("tool-result document: functionResponse %v, inline pdf %v — want both", haveResp, haveBlob)
	}
}
