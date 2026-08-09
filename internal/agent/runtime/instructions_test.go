package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadInstructions(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	// Neither file present → empty.
	if got := loadInstructions(root, home); got != "" {
		t.Errorf("no MEMCODE.md should yield no instructions, got %q", got)
	}

	// Project-only.
	if err := os.WriteFile(filepath.Join(root, memcodeMdName), []byte("Always run go test before push.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadInstructions(root, home)
	if !strings.Contains(got, "Always run go test before push.") {
		t.Errorf("project instructions missing: %q", got)
	}
	if !strings.Contains(got, "PROJECT INSTRUCTIONS") {
		t.Errorf("missing the standing-doctrine header: %q", got)
	}

	// Global + project: both present, global (user-wide) before project (more specific).
	gdir := filepath.Join(home, ".memcode")
	if err := os.MkdirAll(gdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gdir, memcodeMdName), []byte("Prefer tabs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = loadInstructions(root, home)
	gi, pi := strings.Index(got, "Prefer tabs."), strings.Index(got, "Always run go test before push.")
	if gi < 0 || pi < 0 {
		t.Fatalf("both sources should be present, got %q", got)
	}
	if gi > pi {
		t.Errorf("user-wide instructions should come before project ones, got %q", got)
	}

	// Empty/whitespace-only file is ignored (not a blank labeled section).
	if err := os.WriteFile(filepath.Join(root, memcodeMdName), []byte("   \n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadInstructions(root, home); strings.Contains(got, "Project instructions (./"+memcodeMdName+")") {
		t.Errorf("a blank project file should not add a MEMCODE.md section, got %q", got)
	}
}

func TestLoadInstructionsFallsThroughToClaudeMd(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	// No MEMCODE.md, but a CLAUDE.md exists → use it.
	if err := os.WriteFile(filepath.Join(root, claudeMdName), []byte("Use 2-space indent.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadInstructions(root, home)
	if !strings.Contains(got, "Use 2-space indent.") || !strings.Contains(got, "./"+claudeMdName) {
		t.Errorf("should fall through to CLAUDE.md when MEMCODE.md is absent, got %q", got)
	}

	// MEMCODE.md present → it WINS; CLAUDE.md is not also loaded at the same scope.
	if err := os.WriteFile(filepath.Join(root, memcodeMdName), []byte("Use tabs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = loadInstructions(root, home)
	if !strings.Contains(got, "Use tabs.") {
		t.Errorf("MEMCODE.md should win over CLAUDE.md, got %q", got)
	}
	if strings.Contains(got, "Use 2-space indent.") {
		t.Errorf("CLAUDE.md must not also load when MEMCODE.md exists at the same scope, got %q", got)
	}
}

func TestInstructionTier(t *testing.T) {
	cases := []struct {
		n    int
		want tier
	}{
		{0, tierLoad},
		{shrinkwrapBytes, tierLoad},
		{shrinkwrapBytes + 1, tierShrink},
		{refuseBytes, tierShrink},
		{refuseBytes + 1, tierRefuse},
	}
	for _, c := range cases {
		if got := instructionTier(c.n); got != c.want {
			t.Errorf("instructionTier(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

func TestShrinkwrapCachePathStableAndHashed(t *testing.T) {
	root := t.TempDir()
	a := shrinkwrapCachePath(root, "rule one\nrule two")
	again := shrinkwrapCachePath(root, "rule one\nrule two")
	b := shrinkwrapCachePath(root, "rule one\nrule THREE")
	if a != again {
		t.Errorf("same content must map to the same cache path: %q vs %q", a, again)
	}
	if a == b {
		t.Errorf("different content must map to different cache paths (hash-keyed): both %q", a)
	}
	if !strings.Contains(a, filepath.Join(".memcode", "instructions")) {
		t.Errorf("cache must live under .memcode/instructions, got %q", a)
	}
}
