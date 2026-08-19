package vxui

import (
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/jobs"
	"github.com/memcode-ai/memcode/internal/theme"
)

func TestAgentPanelRenders(t *testing.T) {
	theme.Set("aurora")
	s := &appState{sty: makeStyles(theme.Active().Palette)}

	if rows := s.agentPanel(); rows != nil {
		t.Fatalf("no agents → no panel, got %d rows", len(rows))
	}

	s.agentJobs = []jobs.Job{
		{ID: "job_a", Task: "fix flaky test", Status: jobs.StatusRunning, StartedAt: time.Now()},
		{ID: "job_b", Task: "review the diff", Status: jobs.StatusRunning, StartedAt: time.Now()},
	}
	if rows := s.agentPanel(); len(rows) != 2 {
		t.Fatalf("panel rows = %d, want 2 (one per running agent)", len(rows))
	}
}

func TestFormatAgentRow(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 1, 32, 0, time.UTC)
	started := now.Add(-92 * time.Second)

	// No heartbeat yet: activity falls back, no token segment.
	got := formatAgentRow(jobs.Job{Task: "t", StartedAt: started}, now)
	if !strings.Contains(got, "starting…") || strings.Contains(got, "tokens") {
		t.Errorf("pre-heartbeat row wrong: %q", got)
	}
	if !strings.Contains(got, "1m32s") {
		t.Errorf("elapsed missing: %q", got)
	}

	// With a heartbeat: activity + compact token count.
	got = formatAgentRow(jobs.Job{Task: "t", StartedAt: started,
		Activity: "bash(go test ./...)", TokensOut: 4200}, now)
	if !strings.Contains(got, "bash(go test ./...)") || !strings.Contains(got, "↓4.2k tokens") {
		t.Errorf("heartbeat row wrong: %q", got)
	}
}

func TestElapsedShort(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{42 * time.Second, "42s"},
		{92 * time.Second, "1m32s"},
		{3841 * time.Second, "1h04m"},
		{-5 * time.Second, "0s"},
	}
	for _, c := range cases {
		if got := elapsedShort(c.d); got != c.want {
			t.Errorf("elapsedShort(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestLaunchBlock(t *testing.T) {
	if lines := launchBlock(nil); lines != nil {
		t.Fatalf("no fresh jobs → no block, got %v", lines)
	}

	one := launchBlock([]jobs.Job{{Task: "solo task"}})
	if len(one) != 2 || !strings.Contains(one[0], "1 background agent launched") {
		t.Errorf("singular block wrong: %v", one)
	}
	if !strings.Contains(one[1], "└ solo task") {
		t.Errorf("single row should use └: %v", one)
	}

	two := launchBlock([]jobs.Job{{Task: "task a"}, {Task: "task b"}})
	if len(two) != 3 || !strings.Contains(two[0], "2 background agents launched") {
		t.Errorf("plural block wrong: %v", two)
	}
	if !strings.Contains(two[1], "├ task a") || !strings.Contains(two[2], "└ task b") {
		t.Errorf("tree glyphs wrong: %v", two)
	}
}
