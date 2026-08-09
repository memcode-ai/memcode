package runtime

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/input"
)

func TestUserBlocksKeepPastedTextBeforeNativeAttachment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Screenshot.png")
	writeAttachmentTestPNG(t, path)

	text := path + " what does this image show?"
	var out bytes.Buffer
	s := &Session{out: &out}
	blocks := s.userBlocks(context.Background(), input.Bundle{
		Text: text,
		Attachments: []input.Attachment{{
			Path: path,
			Kind: input.KindImage,
			Mime: "image/png",
		}},
	})

	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want text then image", blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != text {
		t.Errorf("first block = %+v, want preserved text block %q", blocks[0], text)
	}
	if blocks[1].Type != "image" {
		t.Errorf("second block type = %q, want image", blocks[1].Type)
	}
}

func TestUserBlocksDoesNotNarrateAttachmentOnlyInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Screenshot.png")
	writeAttachmentTestPNG(t, path)

	var out bytes.Buffer
	s := &Session{out: &out}
	blocks := s.userBlocks(context.Background(), input.Bundle{
		Text:           path,
		AttachmentOnly: true,
		Attachments: []input.Attachment{{
			Path: path,
			Kind: input.KindImage,
			Mime: "image/png",
		}},
	})

	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[0].Text != path || blocks[1].Type != "image" {
		t.Fatalf("blocks = %+v, want original path text followed by image", blocks)
	}
	if got := out.String(); strings.Contains(got, "this paste contains only attachment reference") || strings.Contains(got, "passing the pasted path as text") {
		t.Errorf("output = %q, must not narrate a valid attachment-only submission", got)
	}
}

func TestUserTurnLogTextPreservesTextOrUsesSafeAttachmentMarker(t *testing.T) {
	if got := userTurnLogText(input.Bundle{Text: "  pasted conversation  "}); got != "  pasted conversation  " {
		t.Errorf("canonical text = %q, want original conversation text", got)
	}
	if got := userTurnLogText(input.Bundle{AttachmentOnly: true}); got != "[attachment-only input]" {
		t.Errorf("attachment-only marker = %q", got)
	}
	// With attachments present, the marker says WHAT arrived — count per kind, sorted.
	if got := userTurnLogText(input.Bundle{Attachments: []input.Attachment{
		{Kind: input.KindImage}, {Kind: input.KindPDF}, {Kind: input.KindImage},
	}}); got != "[attachment-only input: 2 image, 1 pdf]" {
		t.Errorf("attachment marker = %q, want counts per kind", got)
	}
	if got := userTurnLogText(input.Bundle{}); got != "" {
		t.Errorf("empty bundle marker = %q, want empty", got)
	}
}

func TestUserBlocksFallBackToAttachmentOnlyTextWhenAttachmentCannotBeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.png")
	text := path
	var out bytes.Buffer
	s := &Session{out: &out}
	blocks := s.userBlocks(context.Background(), input.Bundle{
		Text:           text,
		AttachmentOnly: true,
		Attachments: []input.Attachment{{
			Path: path,
			Kind: input.KindImage,
			Mime: "image/png",
		}},
	})

	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != text {
		t.Fatalf("blocks = %+v, want only the original text path", blocks)
	}
	if got := out.String(); strings.Contains(got, "this paste contains only attachment reference") || strings.Contains(got, "passing the pasted path as text") {
		t.Errorf("output = %q, must not narrate the text fallback", got)
	}
}

func TestUserBlocksAttachmentOnlyWithoutTextDoesNotInjectNarration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Screenshot.png")
	writeAttachmentTestPNG(t, path)

	var out bytes.Buffer
	s := &Session{out: &out}
	blocks := s.userBlocks(context.Background(), input.Bundle{
		AttachmentOnly: true,
		Attachments: []input.Attachment{{
			Path: path,
			Kind: input.KindImage,
			Mime: "image/png",
		}},
	})

	if len(blocks) != 1 || blocks[0].Type != "image" {
		t.Fatalf("blocks = %+v, want only the image", blocks)
	}
	if got := out.String(); strings.Contains(got, "this paste contains only attachment reference") || strings.Contains(got, "passing the pasted path as text") {
		t.Errorf("output = %q, must not inject attachment-only narration", got)
	}
}

func writeAttachmentTestPNG(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.Black)
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
