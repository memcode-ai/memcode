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
// a fake OpenClaw install: channels, provider keys, skills, and memory extracted
// from the workspace markdown into global memory.md.
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

	// Workspace memory: headings for context, bullets and a paragraph as entries,
	// plus a code block and a table row that must be dropped.
	ws := filepath.Join(src, "workspace")
	mustMkdir(t, ws)
	mustWrite(t, filepath.Join(ws, "MEMORY.md"), strings.Join([]string{
		"# Preferences",
		"- Prefers Go over Python",
		"- Uses tabs",
		"",
		"## Editor",
		"Works in Neovim.",
		"",
		"```",
		"do not import this secret",
		"```",
		"| col | col |",
		"",
	}, "\n"))
	// A daily memory file too.
	mustMkdir(t, filepath.Join(ws, "memory"))
	mustWrite(t, filepath.Join(ws, "memory", "2026-08-01.md"), "- Shipped the gateway\n")

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
			channels: openClawChannels,
			memory:   openClawMemory,
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

	// Memory extracted into global memory.md, with heading context and code/table
	// content dropped.
	mem := mustRead(t, filepath.Join(home, ".memcode", "memory.md"))
	for _, want := range []string{
		"Preferences: Prefers Go over Python",
		"Preferences: Uses tabs",
		"Preferences > Editor: Works in Neovim.",
		"Shipped the gateway",
	} {
		if !strings.Contains(mem, want) {
			t.Errorf("memory.md missing entry %q; got:\n%s", want, mem)
		}
	}
	if strings.Contains(mem, "do not import this secret") {
		t.Errorf("code-block content must be dropped; got:\n%s", mem)
	}
	if strings.Contains(mem, "| col |") {
		t.Errorf("table rows must be dropped; got:\n%s", mem)
	}

	// Re-run is idempotent: exactly one import block, not a duplicate.
	run()
	mem = mustRead(t, filepath.Join(home, ".memcode", "memory.md"))
	if n := strings.Count(mem, "memcode:import:openclaw:start"); n != 1 {
		t.Errorf("expected exactly one import block after two runs, got %d:\n%s", n, mem)
	}
}

func TestExtractMarkdownEntries(t *testing.T) {
	entries := extractMarkdownEntries(strings.Join([]string{
		"# Habits",
		"- Wakes at 6am",
		"- Wakes at 6am", // exact duplicate, same context → deduped
		"Runs daily.",
		"## Diet",
		"- Vegetarian",
		"```",
		"code line",
		"```",
		"| a | b |",
	}, "\n"))

	want := []string{
		"Habits: Wakes at 6am",
		"Habits: Runs daily.",
		"Habits > Diet: Vegetarian",
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries %v, want %d %v", len(entries), entries, len(want), want)
	}
	for i, w := range want {
		if entries[i] != w {
			t.Errorf("entry %d = %q, want %q", i, entries[i], w)
		}
	}
}

func TestHermesMemorySplitsOnDelimiter(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "memories"))
	mustWrite(t, filepath.Join(dir, "memories", "MEMORY.md"),
		"First fact\n§\nSecond fact\n§\n  \n§\nThird fact")
	got := hermesMemory(dir)
	want := []string{"First fact", "Second fact", "Third fact"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("hermesMemory = %v, want %v", got, want)
	}
}

func TestUpsertMemoryBlockPreservesUserContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.md")
	mustWrite(t, path, "My own note.\n")

	if err := upsertMemoryBlock(path, "openclaw", "<!-- memcode:import:openclaw:start -->\nA\n<!-- memcode:import:openclaw:end -->"); err != nil {
		t.Fatal(err)
	}
	// Replace it; the user's note and single-block invariant hold.
	if err := upsertMemoryBlock(path, "openclaw", "<!-- memcode:import:openclaw:start -->\nB\n<!-- memcode:import:openclaw:end -->"); err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, path)
	if !strings.Contains(got, "My own note.") {
		t.Errorf("user content lost: %q", got)
	}
	if strings.Contains(got, "\nA\n") {
		t.Errorf("stale block not replaced: %q", got)
	}
	if n := strings.Count(got, "memcode:import:openclaw:start"); n != 1 {
		t.Errorf("expected one block, got %d: %q", n, got)
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

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
