package anthropic

// Document-input mapping tests relocated with the adapter extraction.

import (
	"encoding/base64"
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

func TestAnthropicToolResultDocument(t *testing.T) {
	tr := blockToParam(toolResultWithDoc("tu_1")).OfToolResult
	if tr == nil {
		t.Fatal("expected a tool_result block")
	}
	var found bool
	for _, p := range tr.Content {
		if p.OfDocument != nil && p.OfDocument.Source.OfBase64 != nil &&
			p.OfDocument.Source.OfBase64.Data == base64.StdEncoding.EncodeToString(pdfBytes) {
			found = true
		}
	}
	if !found {
		t.Fatal("document content block did not reach the tool_result content union")
	}
}
