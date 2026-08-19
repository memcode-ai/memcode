package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateScheduleSpec(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name             string
		cron, every, at  string
		wantErr, wantsAt bool
	}{
		{"none set", "", "", "", true, false},
		{"two set", "0 9 * * 1-5", "30m", "", true, false},
		{"good cron", "0 9 * * 1-5", "", "", false, false},
		{"bad cron", "not a cron", "", "", true, false},
		{"good every", "", "30m", "", false, false},
		{"bad every", "", "monthly", "", true, false},
		{"good at duration", "", "", "2h", false, true},
		{"at in the past", "", "", "2001-01-01T09:00", true, false},
		{"bad at", "", "", "whenever", true, false},
	}
	for _, tc := range cases {
		at, err := ValidateScheduleSpec(tc.cron, tc.every, tc.at, now)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr=%v", tc.name, err, tc.wantErr)
		}
		if tc.wantsAt && at == "" {
			t.Errorf("%s: expected a resolved at timestamp", tc.name)
		}
	}
	// The at form resolves to absolute RFC3339 so what fires is what was meant.
	at, err := ValidateScheduleSpec("", "", "2h", now)
	if err != nil {
		t.Fatal(err)
	}
	when, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatalf("resolved at %q is not RFC3339: %v", at, err)
	}
	if d := when.Sub(now.Add(2 * time.Hour)); d < -time.Minute || d > time.Minute {
		t.Errorf("resolved at %q, want ~now+2h", at)
	}
}

func TestValidateDeliverTo(t *testing.T) {
	for _, bad := range []string{"", "telegram", "telegram:", ":123456"} {
		if err := ValidateDeliverTo(bad); err == nil {
			t.Errorf("ValidateDeliverTo(%q) accepted a bad address", bad)
		}
	}
	if err := ValidateDeliverTo("telegram:123456"); err != nil {
		t.Errorf("ValidateDeliverTo(telegram:123456): %v", err)
	}
}

func TestBuildScheduleAndAdd(t *testing.T) {
	now := time.Now()
	// Every field — including Agent — must land on the built schedule.
	sc, err := BuildSchedule(" standup ", "0 9 * * 1-5", "", "", "America/Los_Angeles",
		" summarize commits ", "telegram:123456", " researcher ", now)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Name != "standup" || sc.Task != "summarize commits" || sc.Agent != "researcher" ||
		sc.TZ != "America/Los_Angeles" || sc.DeliverTo != "telegram:123456" || sc.Cron != "0 9 * * 1-5" {
		t.Errorf("BuildSchedule dropped or mangled a field: %+v", sc)
	}

	// Everything the CLI refuses must be refused here too (single source of truth).
	if _, err := BuildSchedule("x", "", "", "", "", "", "telegram:1", "", now); err == nil {
		t.Error("empty task accepted")
	}
	if _, err := BuildSchedule("", "0 9 * * *", "", "", "", "t", "telegram:1", "", now); err == nil {
		t.Error("empty name accepted")
	}
	if _, err := BuildSchedule("x", "bad", "", "", "", "t", "telegram:1", "", now); err == nil {
		t.Error("bad cron accepted")
	}
	if _, err := BuildSchedule("x", "0 9 * * *", "", "", "", "t", "telegram", "", now); err == nil {
		t.Error("bad deliver_to accepted")
	}
	if _, err := BuildSchedule("x", "", "", "", "", "t", "telegram:1", "", now); err == nil {
		t.Error("no timing form accepted")
	}

	var s Settings
	if err := s.AddSchedule(sc); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := s.AddSchedule(sc); err == nil {
		t.Error("duplicate name accepted — must be an explicit remove-then-add")
	}
	if len(s.Schedules) != 1 {
		t.Errorf("schedules = %d, want 1", len(s.Schedules))
	}
}

func TestRegisterProject(t *testing.T) {
	real := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "lnk")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realResolved, _ := filepath.EvalSymlinks(real)

	var s Settings
	// Id derived from the CANONICAL directory's name; path symlink-resolved.
	id, root, err := s.RegisterProject("", link)
	if err != nil {
		t.Fatal(err)
	}
	if id != "app" {
		t.Errorf("derived id = %q, want app (from the resolved root)", id)
	}
	if root != realResolved {
		t.Errorf("root = %q, want canonical %q", root, realResolved)
	}
	if s.DefaultProject != "app" {
		t.Errorf("first project must become the default, got %q", s.DefaultProject)
	}

	// Re-adding the same directory is idempotent.
	if _, _, err := s.RegisterProject("app", link); err != nil {
		t.Errorf("re-add of the same path refused: %v", err)
	}

	// An id collision with a DIFFERENT path is refused, never overwritten.
	other := t.TempDir()
	if _, _, err := s.RegisterProject("app", other); err == nil {
		t.Error("id collision with a different path was silently accepted")
	} else if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("collision error should say so, got: %v", err)
	}
	if got, _ := filepath.EvalSymlinks(s.Projects["app"].Path); got != realResolved {
		t.Errorf("collision overwrote the registered path: %q", s.Projects["app"].Path)
	}

	// A non-directory is refused.
	f := filepath.Join(real, "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterProject("", f); err == nil {
		t.Error("a plain file was accepted as a project root")
	}

	// A second project does not steal the default.
	if _, _, err := s.RegisterProject("other", other); err != nil {
		t.Fatal(err)
	}
	if s.DefaultProject != "app" {
		t.Errorf("default moved to %q on a later registration", s.DefaultProject)
	}
}
