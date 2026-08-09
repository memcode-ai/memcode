package runtime

import (
	"bytes"
	"strings"
	"testing"
)

// TestQuietToolLinesPrintMutedBullet: read-only research tools (Read/List/Search/Glob) pass
// shown=false — they print exactly one ● line (dim bullet, never the loud ⏺), so the user
// can follow what's being read without research competing with loud actions. Dropping them
// entirely was a regression: useful Read/List activity vanished from the transcript.
func TestQuietToolLinesPrintMutedBullet(t *testing.T) {
	var out bytes.Buffer
	s := &Session{out: &out}
	s.toolLine(false, "Read", "internal/x.go", "42 lines", false)
	got := out.String()
	if !strings.Contains(got, "●") || strings.Contains(got, "⏺") {
		t.Errorf("a quiet tool line must print with the ● bullet, never ⏺: %q", got)
	}
	if !strings.Contains(got, "Read(internal/x.go)") {
		t.Errorf("the quiet line must carry the verb+arg label: %q", got)
	}
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("exactly one line expected, got %d: %q", n, got)
	}
}

// A quiet FAILURE must still be noticeable: the bullet takes the failure color, not Faint.
func TestQuietToolFailureKeepsFailColor(t *testing.T) {
	var out bytes.Buffer
	s := &Session{out: &out}
	s.toolLineStat(false, "List", "internal", "3 entries", statFail)
	got := out.String()
	if !strings.Contains(got, "●") {
		t.Fatalf("quiet failure still prints the ● bullet: %q", got)
	}
	if strings.Contains(got, "\x1b[2m●") {
		t.Errorf("a failed quiet line must not render the bullet Faint — it takes the failure color: %q", got)
	}
}

// A loud (shown=true) tool line is unaffected — it must still reach the writer.
func TestLoudToolLinesStillPrint(t *testing.T) {
	var out bytes.Buffer
	s := &Session{out: &out}
	s.toolLine(true, "Bash", "ls", "", false)
	if !strings.Contains(out.String(), "Bash(ls)") {
		t.Errorf("a loud (shown=true) tool line must still print, got %q", out.String())
	}
}

func TestOSC8FileLink(t *testing.T) {
	got := osc8FileLink("/repo", "internal/x.go", "internal/x.go")
	// Well-formed OSC 8: ESC]8;;<uri>ST <text> ESC]8;;ST
	if !strings.HasPrefix(got, "\x1b]8;;file:///repo/internal/x.go\x1b\\") {
		t.Errorf("must open an OSC 8 link to the absolute file:// URI: %q", got)
	}
	if !strings.HasSuffix(got, "\x1b]8;;\x1b\\") {
		t.Errorf("must close the OSC 8 link: %q", got)
	}
	if !strings.Contains(got, "internal/x.go\x1b]8;;") { // the visible text sits between open and close
		t.Errorf("visible text must be preserved: %q", got)
	}
	// An already-absolute path is used as-is (not re-joined to root).
	if abs := osc8FileLink("/repo", "/tmp/y.go", "y.go"); !strings.Contains(abs, "file:///tmp/y.go\x1b\\") {
		t.Errorf("absolute path should pass through: %q", abs)
	}
	// Spaces are percent-encoded so the URI stays valid.
	if sp := osc8FileLink("/r", "a b.go", "a b.go"); !strings.Contains(sp, "file:///r/a%20b.go") {
		t.Errorf("spaces must be encoded in the URI: %q", sp)
	}
}
