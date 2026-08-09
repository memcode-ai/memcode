package input

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pasting a screenshot on macOS drops its absolute path (with shell-escaped spaces)
// into the composer; Parse should preserve it as canonical text and add an image attachment.
func TestParseExtractsPastedScreenshotPath(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "Screenshot 2026-06-04 at 5.32.32 PM.png")
	if err := os.WriteFile(img, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	escaped := strings.ReplaceAll(img, " ", `\ `) // how a terminal pastes it

	dec := Parse(escaped+" can you tell me what this says?", dir)

	if dec.Route != Steer {
		t.Errorf("route = %v, want steer", dec.Route)
	}
	wantText := escaped + " can you tell me what this says?"
	if dec.Bundle.Text != wantText {
		t.Errorf("text = %q, want preserved pasted text %q", dec.Bundle.Text, wantText)
	}
	if dec.Bundle.AttachmentOnly {
		t.Error("AttachmentOnly = true, want false for mixed text and attachment input")
	}
	if len(dec.Bundle.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want 1 image", dec.Bundle.Attachments)
	}
	if a := dec.Bundle.Attachments[0]; a.Kind != KindImage || a.Path != img {
		t.Errorf("attachment = %+v, want image at %q", a, img)
	}
}

func TestParsePreservesMixedTranscriptAndAllNativeAttachments(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "IMG_1001.png")
	second := filepath.Join(dir, "IMG_1002.png")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	transcript := strings.Join([]string{
		"Alice: here are the screenshots from yesterday",
		"Alice: " + first,
		"Bob: I can see the issue in the first one.",
		"Alice: " + second,
		"Bob: and the text after the attachments must reach the model too.",
	}, "\n")
	dec := Parse(transcript, dir)

	if dec.Bundle.Text != transcript {
		t.Errorf("text = %q, want complete unmodified transcript %q", dec.Bundle.Text, transcript)
	}
	if dec.Bundle.AttachmentOnly {
		t.Error("AttachmentOnly = true, want false for transcript with message text")
	}
	if len(dec.Bundle.Attachments) != 2 {
		t.Fatalf("attachments = %+v, want both images", dec.Bundle.Attachments)
	}
	for i, want := range []string{first, second} {
		if got := dec.Bundle.Attachments[i]; got.Path != want || got.Kind != KindImage {
			t.Errorf("attachment[%d] = %+v, want image at %q", i, got, want)
		}
	}
}

// ImageMatches returns the exact dragged-in image-path substrings (escapes intact) so
// the TUI can collapse each to an [Image #N] chip. It shares Parse's eligibility
// criteria, so the chip and native attachment never disagree.
func TestImageMatches(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "Screenshot 2026-06-04 at 5.32.32 PM.png")
	if err := os.WriteFile(img, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	escaped := strings.ReplaceAll(img, " ", `\ `)

	got := ImageMatches(escaped+" what is this?", dir)
	if len(got) != 1 || got[0] != escaped {
		t.Fatalf("ImageMatches = %q, want [%q] (exact substring, escapes intact)", got, escaped)
	}
	// Non-existent path and a non-image file must NOT match (no chip for those).
	if m := ImageMatches("/nope/missing.png here", dir); len(m) != 0 {
		t.Errorf("ImageMatches on a non-existent path = %q, want none", m)
	}
}

func TestParseMarksOnlyRecognizedAttachments(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		file string
		data []byte
		kind Kind
	}{
		{name: "image", file: "Screenshot.png", data: []byte("\x89PNG\r\n\x1a\n"), kind: KindImage},
		{name: "pdf", file: "conversation.pdf", data: []byte("%PDF-1.7\n"), kind: KindPDF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, tc.data, 0o644); err != nil {
				t.Fatal(err)
			}

			dec := Parse(path, dir)
			if dec.Bundle.Text != path {
				t.Errorf("text = %q, want original attachment path %q", dec.Bundle.Text, path)
			}
			if !dec.Bundle.AttachmentOnly {
				t.Error("AttachmentOnly = false, want true for a recognized attachment path alone")
			}
			if len(dec.Bundle.Attachments) != 1 {
				t.Fatalf("attachments = %+v, want 1 attachment", dec.Bundle.Attachments)
			}
			if got := dec.Bundle.Attachments[0].Kind; got != tc.kind {
				t.Errorf("attachment kind = %q, want %q", got, tc.kind)
			}
		})
	}
}

// A path-shaped token to a file that doesn't exist (or a non-image) must NOT become
// an attachment, and prose is never scanned — text-is-text holds.
func TestParseLeavesNonexistentAndProseAlone(t *testing.T) {
	dir := t.TempDir()
	missingTranscript := strings.Join([]string{
		"Alice: I tried to send this screenshot:",
		"Alice: /var/tmp/nope-9f3a2b.png",
		"Bob: the rest of this conversation must remain as text.",
	}, "\n")
	if dec := Parse(missingTranscript, dir); len(dec.Bundle.Attachments) != 0 {
		t.Errorf("nonexistent path should not attach: %+v", dec.Bundle.Attachments)
	} else if dec.Bundle.AttachmentOnly {
		t.Error("nonexistent path must not be marked attachment-only")
	} else if dec.Bundle.Text != missingTranscript {
		t.Errorf("text = %q, want complete text-only transcript %q", dec.Bundle.Text, missingTranscript)
	}
	// Prose that happens to contain repo-pathy words must remain ordinary text too.
	if dec := Parse("update the datasets/ loader in scripts and rerun", dir); len(dec.Bundle.Attachments) != 0 {
		t.Errorf("prose should never attach: %+v", dec.Bundle.Attachments)
	}
}
