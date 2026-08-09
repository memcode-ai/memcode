package runtime

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpillReportOverThreshold: an oversized sub-agent report is written whole
// to the session's reports dir and the inline result becomes digest + a
// read_file pointer (so the full text participates in range reads/eviction).
func TestSpillReportOverThreshold(t *testing.T) {
	root := t.TempDir()
	s := &Session{root: root, sessionID: "sess_spill", out: io.Discard, turn: newTurnState()}

	long := strings.Repeat("finding line about the gateway auth cache\n", 1000) // ~43KB
	got := s.spillReport("explore-api", long)

	if len(got) >= len(long) {
		t.Fatalf("spilled result should be a digest, got %dB of %dB", len(got), len(long))
	}
	if !strings.Contains(got, "read_file") || !strings.Contains(got, "reports/") {
		t.Fatalf("digest must carry a read_file pointer to the full report: %q", tail(got, 200))
	}
	// The full text is on disk, byte for byte.
	path := filepath.Join(root, ".memcode", "sessions", "sess_spill", "reports", "001-explore-api.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("full report not persisted: %v", err)
	}
	if string(b) != long {
		t.Fatalf("persisted report differs from the original (%dB vs %dB)", len(b), len(long))
	}
	// A second spill gets its own sequence number.
	_ = s.spillReport("agent-fast", long)
	if _, err := os.Stat(filepath.Join(root, ".memcode", "sessions", "sess_spill", "reports", "002-agent-fast.md")); err != nil {
		t.Fatalf("second report not sequenced: %v", err)
	}
}

// TestSpillReportPassThrough: small reports and failure cases pass through
// untouched — spilling is an optimization, never a failure mode.
func TestSpillReportPassThrough(t *testing.T) {
	s := &Session{root: t.TempDir(), sessionID: "sess_x", out: io.Discard, turn: newTurnState()}
	small := "short report"
	if got := s.spillReport("explore", small); got != small {
		t.Fatalf("under-threshold report must pass through, got %q", got)
	}
	// No session id (headless odd case) → pass through even when huge.
	s2 := &Session{root: t.TempDir(), out: io.Discard, turn: newTurnState()}
	long := strings.Repeat("x", reportSpillThreshold+1)
	if got := s2.spillReport("explore", long); got != long {
		t.Fatal("no-session report must pass through")
	}
}
