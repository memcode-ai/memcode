package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// sanitizeTermOutput keeps SGR color but strips screen/cursor control so a TUI app's escapes
// (run in the $ lane) can't corrupt memcode's own display.
func TestSanitizeTermOutput(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"keeps SGR color", "\x1b[38;2;1;2;3mhi\x1b[0m", "\x1b[38;2;1;2;3mhi\x1b[0m"},
		{"strips cursor move + clear", "a\x1b[2Jb\x1b[10;5Hc", "abc"},
		{"strips alt-screen toggles", "\x1b[?1049hX\x1b[?1049l", "X"},
		{"strips OSC title (BEL)", "\x1b]0;my title\x07ok", "ok"},
		{"strips OSC (ST)", "\x1b]8;;http://x\x1b\\link", "link"},
		{"drops CR, keeps LF + TAB", "a\rb\tc\nd", "ab\tc\nd"},
		{"plain passes through", "hello world", "hello world"},
		{"utf8 multibyte safe", "héllo→λ", "héllo→λ"},
		{"truncated CSI at end dropped", "ok\x1b[10", "ok"},
	}
	for _, c := range cases {
		if got := sanitizeTermOutput(c.in); got != c.want {
			t.Errorf("%s: sanitizeTermOutput(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// abnormalExitReason turns a killed/failed command into a legible reason instead of "exit -1".
func TestAbnormalExitReason(t *testing.T) {
	bg := context.Background()
	if r := abnormalExitReason(bg, nil, 0); r != "" {
		t.Errorf("normal exit → empty, got %q", r)
	}
	if r := abnormalExitReason(bg, errors.New("exit status 1"), 1); r != "" {
		t.Errorf("plain non-zero exit → empty (use the exit code), got %q", r)
	}
	cctx, cancel := context.WithCancel(bg)
	cancel()
	if r := abnormalExitReason(cctx, context.Canceled, -1); r != "interrupted" {
		t.Errorf("cancelled → %q, want interrupted", r)
	}
	dctx, dcancel := context.WithTimeout(bg, 0)
	defer dcancel()
	<-dctx.Done()
	if r := abnormalExitReason(dctx, context.DeadlineExceeded, -1); !strings.HasPrefix(r, "timed out") {
		t.Errorf("deadline → %q, want timed out…", r)
	}
	if r := abnormalExitReason(bg, errors.New(`exec: "foo": not found`), -1); !strings.HasPrefix(r, "could not run") {
		t.Errorf("start failure → %q, want could not run…", r)
	}
}
