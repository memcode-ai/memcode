package runtime

import (
	"strings"
	"testing"
)

func TestThinText(t *testing.T) {
	// Big HTML page that yielded only a title → client-rendered, should escalate.
	if !thinText("Recursive Self-Improvement | Anthropic", 235000) {
		t.Error("title-only on a 235KB page must be flagged as JS-rendered")
	}
	// A text-rich article → not flagged.
	if thinText(strings.Repeat("real article prose ", 300), 235000) {
		t.Error("a text-rich page must NOT be flagged")
	}
	// A genuinely small static page → not flagged (conservative).
	if thinText("ok", 300) {
		t.Error("a tiny page must NOT be flagged")
	}
}
