package artifacts

import (
	"strings"
	"testing"
)

func TestRenderListAlignment(t *testing.T) {
	out := RenderList([]Artifact{
		{ID: "abc123", Title: "Audit Report", URL: "https://memcode.ai/code/artifact/abc123", UpdatedAt: "2026-08-19"},
		{ID: "d4", Title: "", URL: "https://memcode.ai/code/artifact/d4"},
	})
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), out)
	}
	// Columns align: the id starts at the same offset on both lines.
	if strings.Index(lines[0], "abc123") != strings.Index(lines[1], "d4") {
		t.Errorf("id column misaligned:\n%s", out)
	}
	if !strings.Contains(lines[1], "untitled") {
		t.Errorf("empty title should render as untitled: %q", lines[1])
	}
	if !strings.Contains(lines[0], "2026-08-19") {
		t.Errorf("updated_at missing: %q", lines[0])
	}
}

func TestRenderListEmpty(t *testing.T) {
	if out := RenderList(nil); out != "" {
		t.Errorf("want empty string for no artifacts, got %q", out)
	}
}
