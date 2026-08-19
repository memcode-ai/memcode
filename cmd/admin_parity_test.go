package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// The admin tools and the CLI must validate schedule mutations IDENTICALLY —
// both go through gwconfig.BuildSchedule/AddSchedule, so what one surface
// refuses the other refuses too, and the admin path carries every field
// (including the agent pin) the CLI does.
func TestAdminScheduleValidatesLikeCLI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()

	add := func(fields map[string]string) error {
		in := map[string]string{"action": "add"}
		for k, v := range fields {
			in[k] = v
		}
		_, err := adminExecute(ctx, tools.GwSchedule, adminIn(t, in))
		return err
	}

	// Everything the CLI's validation refuses, the admin path refuses too.
	rejects := []struct {
		name   string
		fields map[string]string
	}{
		{"bad cron", map[string]string{"name": "x", "cron": "not a cron", "task": "t", "deliver_to": "telegram:1"}},
		{"bad every", map[string]string{"name": "x", "every": "monthly", "task": "t", "deliver_to": "telegram:1"}},
		{"at in the past", map[string]string{"name": "x", "at": "2001-01-01T09:00", "task": "t", "deliver_to": "telegram:1"}},
		{"two timing forms", map[string]string{"name": "x", "cron": "0 9 * * *", "every": "30m", "task": "t", "deliver_to": "telegram:1"}},
		{"no timing form", map[string]string{"name": "x", "task": "t", "deliver_to": "telegram:1"}},
		{"missing deliver_to", map[string]string{"name": "x", "cron": "0 9 * * *", "task": "t"}},
		{"malformed deliver_to", map[string]string{"name": "x", "cron": "0 9 * * *", "task": "t", "deliver_to": "telegram"}},
		{"missing task", map[string]string{"name": "x", "cron": "0 9 * * *", "deliver_to": "telegram:1"}},
	}
	for _, tc := range rejects {
		if err := add(tc.fields); err == nil {
			t.Errorf("%s: admin path accepted what the CLI refuses", tc.name)
		}
	}

	// A valid add persists every field — Agent included (it used to be dropped).
	if err := add(map[string]string{
		"name": "digest", "every": "24h", "task": "summarize",
		"deliver_to": "telegram:123", "agent": "researcher",
	}); err != nil {
		t.Fatal(err)
	}
	settings, err := gwconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Schedules) != 1 {
		t.Fatalf("schedules = %+v, want 1", settings.Schedules)
	}
	sc := settings.Schedules[0]
	if sc.Agent != "researcher" || sc.Every != "24h" || sc.DeliverTo != "telegram:123" {
		t.Errorf("admin add dropped a field: %+v", sc)
	}

	// Duplicate names are refused, same as the CLI.
	if err := add(map[string]string{
		"name": "digest", "every": "1h", "task": "again", "deliver_to": "telegram:123",
	}); err == nil {
		t.Error("duplicate schedule name accepted")
	}

	// The at form resolves to an absolute RFC3339 timestamp, same as the CLI.
	if err := add(map[string]string{
		"name": "oneshot", "at": "2h", "task": "remind", "deliver_to": "telegram:123",
	}); err != nil {
		t.Fatal(err)
	}
	settings, _ = gwconfig.Load()
	for _, s := range settings.Schedules {
		if s.Name == "oneshot" && !strings.Contains(s.At, "T") {
			t.Errorf("at not resolved to RFC3339: %q", s.At)
		}
	}
}

// Admin gw_project add must register like the CLI: canonical symlink-resolved
// root, and an id collision with a different path is refused, never
// silently overwritten.
func TestAdminProjectRegistersLikeCLI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()

	real := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "lnk")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realResolved, _ := filepath.EvalSymlinks(real)

	if _, err := adminExecute(ctx, tools.GwProject, adminIn(t, map[string]string{"action": "add", "path": link})); err != nil {
		t.Fatal(err)
	}
	settings, err := gwconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := settings.Projects["app"]
	if !ok {
		t.Fatalf("project not registered under derived id: %+v", settings.Projects)
	}
	if p.Path != realResolved {
		t.Errorf("path = %q, want canonical %q (symlink must be resolved)", p.Path, realResolved)
	}
	if settings.DefaultProject != "app" {
		t.Errorf("first project must become the default, got %q", settings.DefaultProject)
	}

	// Same id, different path → refused; the registration is untouched.
	other := t.TempDir()
	if _, err := adminExecute(ctx, tools.GwProject, adminIn(t, map[string]string{"action": "add", "path": other, "id": "app"})); err == nil {
		t.Error("id collision with a different path was silently accepted")
	}
	settings, _ = gwconfig.Load()
	if settings.Projects["app"].Path != realResolved {
		t.Errorf("collision overwrote the registered path: %q", settings.Projects["app"].Path)
	}

	// A non-directory is refused.
	f := filepath.Join(real, "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := adminExecute(ctx, tools.GwProject, adminIn(t, map[string]string{"action": "add", "path": f})); err == nil {
		t.Error("a plain file was accepted as a project root")
	}
}

// clipCLI must never split a multibyte rune: clipped output stays valid UTF-8.
func TestClipCLIRuneSafe(t *testing.T) {
	s := strings.Repeat("héllo→世界 ", 40)
	for max := 1; max < 60; max++ {
		got := clipCLI(s, max)
		if !utf8.ValidString(got) {
			t.Fatalf("clipCLI(max=%d) produced invalid UTF-8: %q", max, got)
		}
	}
	if got := clipCLI("first\nsecond", 100); got != "first …" {
		t.Errorf("newline clip = %q, want %q", got, "first …")
	}
	if got := clipCLI("short", 100); got != "short" {
		t.Errorf("unclipped = %q", got)
	}
}
