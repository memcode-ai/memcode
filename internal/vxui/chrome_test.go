package vxui

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/runtime"
)

// TestStatusStripHiddenWhilePrompting locks the fix for the misleading "Thinking… 51m" clock:
// the live status strip is suppressed whenever a question or approval card is awaiting the user,
// because the turn is blocked on input, not computing.
func TestStatusStripHiddenWhilePrompting(t *testing.T) {
	s := &appState{}
	if !s.statusStripVisible() {
		t.Fatal("status strip should show when nothing is awaiting the user")
	}
	s.pending = &runtime.ApprovalRequest{} // an approval card is up
	if s.statusStripVisible() {
		t.Fatal("status strip must be hidden while an approval card awaits the user")
	}
	s.pending = nil
	s.askReq = &runtime.AskRequest{} // an AskUserQuestion is up
	if s.statusStripVisible() {
		t.Fatal("status strip must be hidden while a question awaits the user")
	}
}

// cacheHitRate is the footer/status cache-hit % — read/(input+read+write), div-by-zero-safe.
func TestCacheHitRate(t *testing.T) {
	cases := []struct {
		in, read, write int
		want            float64
	}{
		{0, 0, 0, 0},             // nothing cached → 0, no divide-by-zero
		{50, 0, 0, 0},            // no cache reads → 0%
		{100, 0, 10000, 0},       // Anthropic cold-cache write → 0%
		{1, 2421, 0, 99.9587},    // Gemini/OpenAI warm cache
		{12, 93571, 15693, 85.6}, // Anthropic warm cache with cache_write in denominator
		{0, 100, 0, 100},         // fully cached
		{100, 100, 0, 50},        // half
	}
	for _, c := range cases {
		got := cacheHitRate(c.in, c.read, c.write)
		if got < c.want-0.05 || got > c.want+0.05 {
			t.Errorf("cacheHitRate(in=%d, read=%d, write=%d) = %.4f, want ~%.4f", c.in, c.read, c.write, got, c.want)
		}
	}
}

// ctxPercent is the footer context-fill % — tokens*100/window, div-by-zero-safe,
// deliberately unclamped above 100 (an honest overflow warning).
func TestCtxPercent(t *testing.T) {
	cases := []struct{ tokens, window, want int }{
		{0, 0, 0},               // window unknown → 0, no divide-by-zero
		{5000, 0, 0},            // window unknown even with tokens
		{0, 100000, 0},          // no call yet
		{34000, 100000, 34},     // ordinary fill
		{80000, 100000, 80},     // the warn threshold boundary
		{1200000, 1000000, 120}, // over the window → >100, unclamped
	}
	for _, c := range cases {
		if got := ctxPercent(c.tokens, c.window); got != c.want {
			t.Errorf("ctxPercent(tokens=%d, window=%d) = %d, want %d", c.tokens, c.window, got, c.want)
		}
	}
}
