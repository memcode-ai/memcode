package channels

import (
	"strings"
	"testing"
)

func TestChunk(t *testing.T) {
	// Short strings pass through as one piece.
	if got := Chunk("hello", 2000); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("short: got %v", got)
	}
	// Empty string still yields one (empty) piece.
	if got := Chunk("", 2000); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty: got %v", got)
	}
	// Over-limit input splits into pieces each within the limit, losslessly.
	long := strings.Repeat("a", 4500)
	parts := Chunk(long, 2000)
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(parts))
	}
	if strings.Join(parts, "") != long {
		t.Error("chunking lost or altered content")
	}
	for _, p := range parts {
		if len([]rune(p)) > 2000 {
			t.Errorf("part exceeds limit: %d", len([]rune(p)))
		}
	}
	// Prefers a newline break near the limit over a hard cut, and stays lossless.
	withNL := strings.Repeat("x", 1500) + "\n" + strings.Repeat("y", 1500)
	got := Chunk(withNL, 2000)
	if len(got) != 2 || !strings.HasSuffix(got[0], "\n") {
		t.Errorf("newline break: got %d pieces, first ends nl=%v", len(got), strings.HasSuffix(got[0], "\n"))
	}
	if strings.Join(got, "") != withNL {
		t.Error("newline-break chunking was not lossless")
	}
}
