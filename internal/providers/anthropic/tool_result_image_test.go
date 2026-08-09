package anthropic

import (
	"encoding/base64"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// TestToolResultImageContentBlocks asserts that a tool_result carrying structured
// content (text + image) serializes an image block with a base64 source in the
// Anthropic request — the wire shape for tool results that include vision (e.g.
// a browser screenshot returned to the model).
func TestToolResultImageContentBlocks(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4E, 0x47} // PNG magic bytes
	block := wire.Block{
		Type:      "tool_result",
		ToolUseID: "toolu_01",
		IsError:   false,
		ContentBlocks: []wire.Block{
			wire.TextBlock("screenshot captured"),
			wire.ImageBlock("image/png", png),
		},
	}

	p := blockToParam(block)
	if p.OfToolResult == nil {
		t.Fatal("expected a tool_result param")
	}
	rp := p.OfToolResult
	if rp.ToolUseID != "toolu_01" {
		t.Errorf("tool_use_id mismatch: %s", rp.ToolUseID)
	}
	if len(rp.Content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(rp.Content))
	}

	// First part: text.
	if rp.Content[0].OfText == nil || rp.Content[0].OfText.Text != "screenshot captured" {
		t.Errorf("first part should be text, got %#v", rp.Content[0])
	}

	// Second part: image with base64 source.
	if rp.Content[0].OfImage != nil {
		t.Error("first part should be text, not image")
	}
	if rp.Content[1].OfImage == nil {
		t.Fatalf("second part should be an image block")
	}
	img := rp.Content[1].OfImage
	if img.Source.OfBase64 == nil {
		t.Fatal("image source should be base64")
	}
	if img.Source.OfBase64.Data != base64.StdEncoding.EncodeToString(png) {
		t.Errorf("image data should be base64-encoded PNG, got %s", img.Source.OfBase64.Data)
	}
	if string(img.Source.OfBase64.MediaType) != "image/png" {
		t.Errorf("media_type should be image/png, got %s", img.Source.OfBase64.MediaType)
	}
}

// TestToolResultFlatContentBackcompat asserts the backwards-compatible path: a
// tool_result with only Content (flat string, no ContentBlocks) still emits a
// single text part — the pre-image path every existing tool result uses.
func TestToolResultFlatContentBackcompat(t *testing.T) {
	block := wire.Block{
		Type:      "tool_result",
		ToolUseID: "toolu_02",
		Content:   "exit code: 0\n--- stdout ---\nhello",
	}
	p := blockToParam(block)
	if p.OfToolResult == nil {
		t.Fatal("expected a tool_result param")
	}
	rp := p.OfToolResult
	if len(rp.Content) != 1 || rp.Content[0].OfText == nil {
		t.Fatalf("flat content should be one text part, got %#v", rp.Content)
	}
	if rp.Content[0].OfText.Text != "exit code: 0\n--- stdout ---\nhello" {
		t.Errorf("text mismatch: %s", rp.Content[0].OfText.Text)
	}
}
