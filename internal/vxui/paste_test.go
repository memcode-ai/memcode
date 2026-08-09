package vxui

import (
	"fmt"
	"strings"
	"testing"
)

// The load-bearing guarantee: once submitted, a paste resolves to its FULL content internally —
// never the bare token, never a clipped preview. Regression for the AskUser path shipping the
// "[pasted …]" placeholder to the model.
func TestExpandComposerFullContent(t *testing.T) {
	s := &appState{}
	full := strings.TrimRight(strings.Repeat("option line\n", 200), "\n") // multi-line; a preview would clip it
	s.pastes = map[string]string{"[pasted #1 2.3KB]": full}
	s.composer = "I would do all of these [pasted #1 2.3KB]"
	got := s.expandComposer()
	if !strings.Contains(got, full) {
		t.Fatalf("expandComposer must yield the FULL paste content, not clipped (len got=%d, want≥%d)", len(got), len(full))
	}
	if strings.Contains(got, "[pasted #") {
		t.Errorf("the token must be expanded away, got prefix %q", got[:min(40, len(got))])
	}
}

// clipPasteMiddle elides the MIDDLE (first N + skip marker + last N), keeping both ends — this is
// display only and must never be what reaches the engine.
func TestClipPasteMiddle(t *testing.T) {
	short := "a\nb\nc"
	if got := strings.Join(clipPasteMiddle(short, 80), "\n"); got != short {
		t.Errorf("short paste must pass through whole, got %q", got)
	}
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, fmt.Sprintf("L%d", i))
	}
	got := clipPasteMiddle(strings.Join(lines, "\n"), 80)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "L0") || !strings.Contains(joined, "L59") {
		t.Errorf("middle elision must keep the first and last lines, got %v", got)
	}
	if !strings.Contains(joined, "lines skipped") {
		t.Errorf("expected a middle skip marker, got %v", got)
	}
	if len(got) >= len(lines) {
		t.Errorf("middle elision must shrink the output, got %d of %d lines", len(got), len(lines))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
