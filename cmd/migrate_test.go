package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRunMigrationOpenClaw exercises the full migration engine end-to-end against
// a fake OpenClaw install: channels, provider keys, skills, and memory.
func TestRunMigrationOpenClaw(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	src := filepath.Join(home, ".openclaw")
	mustMkdir(t, src)
	mustWrite(t, filepath.Join(src, "openclaw.json"),
		`{"channels":{"telegram":{"botToken":"tg-tok","allowFrom":["123"]}}}`)
	mustWrite(t, filepath.Join(src, ".env"), "OPENAI_API_KEY=sk-openai\nRANDOM=nope\n")

	// One real skill (has SKILL.md) and one directory that is not a skill.
	mustMkdir(t, filepath.Join(src, "skills", "hello"))
	mustWrite(t, filepath.Join(src, "skills", "hello", "SKILL.md"), "# hello\n")
	mustMkdir(t, filepath.Join(src, "skills", "notaskill"))

	// A memory/history store.
	mustMkdir(t, filepath.Join(src, "state"))
	mustWrite(t, filepath.Join(src, "state", "openclaw.sqlite"), "db-bytes")

	dir, _ := openClawDir("")
	if dir != src {
		t.Fatalf("openClawDir = %q, want %q", dir, src)
	}

	run := func() string {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		if err := runMigration(cmd, migrationSource{
			display: "OpenClaw", slug: "openclaw", dir: dir,
			channels:        openClawChannels,
			memoryArtifacts: []string{"state", "openclaw.sqlite", "memory"},
		}); err != nil {
			t.Fatalf("runMigration: %v", err)
		}
		return buf.String()
	}

	out := run()

	if !strings.Contains(out, "telegram") {
		t.Errorf("expected telegram channel in output: %q", out)
	}
	if !strings.Contains(out, "1 provider key") {
		t.Errorf("expected the OpenAI key migrated: %q", out)
	}

	// Skill copied; non-skill dir ignored.
	if _, err := os.Stat(filepath.Join(home, ".memcode", "skills", "hello", "SKILL.md")); err != nil {
		t.Errorf("skill not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".memcode", "skills", "notaskill")); !os.IsNotExist(err) {
		t.Error("a directory without SKILL.md must not be imported as a skill")
	}

	// Memory preserved verbatim under imported/<slug>/.
	if _, err := os.Stat(filepath.Join(home, ".memcode", "imported", "openclaw", "state", "openclaw.sqlite")); err != nil {
		t.Errorf("memory not preserved: %v", err)
	}

	// Global memory.md carries a single idempotent pointer, even after a re-run.
	run()
	mm, err := os.ReadFile(filepath.Join(home, ".memcode", "memory.md"))
	if err != nil {
		t.Fatalf("reading memory.md: %v", err)
	}
	if n := strings.Count(string(mm), "imported:openclaw"); n != 1 {
		t.Errorf("expected exactly one memory pointer after two runs, got %d: %q", n, mm)
	}
}

func TestMigrationDirNotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if dir, _ := openClawDir(""); dir != "" {
		t.Errorf("no install should resolve to empty, got %q", dir)
	}
	if dir := hermesDir(""); dir != "" {
		t.Errorf("no Hermes install should resolve to empty, got %q", dir)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
