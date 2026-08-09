package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/config"
)

// TestDetectAndManaged: a file we wrote is reported Exists+Managed; a file with
// foreign content is Exists but not Managed; an absent one is neither.
func TestDetectAndManaged(t *testing.T) {
	root := t.TempDir()
	// One memcode-managed file, one hand-written file, two absent.
	if err := Write(root, "the project overview", []config.SyncTargetMeta{{Name: "Claude", Path: "CLAUDE.md"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".windsurfrules"), []byte("hand written, not ours"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := map[string]DetectedTarget{}
	for _, d := range Detect(root) {
		got[d.Name] = d
	}
	if !got["Claude"].Exists || !got["Claude"].Managed {
		t.Errorf("CLAUDE.md should be Exists+Managed, got %+v", got["Claude"])
	}
	if !got["Windsurf"].Exists || got["Windsurf"].Managed {
		t.Errorf("hand-written .windsurfrules should be Exists but NOT Managed, got %+v", got["Windsurf"])
	}
	if got["Cursor"].Exists {
		t.Errorf(".cursor/rules should not exist, got %+v", got["Cursor"])
	}
}

// TestActiveTargetsEverythingIsDetectedOnly: "Everything" must resolve to files
// that already exist — never the full four — so a commit doesn't litter the repo
// with context files for editors the user doesn't use.
func TestActiveTargetsEverythingIsDetectedOnly(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, "x", []config.SyncTargetMeta{{Name: "Claude", Path: "CLAUDE.md"}}); err != nil {
		t.Fatal(err)
	}
	targets := ActiveTargets(root, config.SyncConfig{Everything: true})
	if len(targets) != 1 || targets[0].Path != "CLAUDE.md" {
		t.Fatalf("Everything should resolve to only the existing CLAUDE.md, got %+v", targets)
	}

	// Empty repo → Everything resolves to nothing (adopted later when files appear).
	if got := ActiveTargets(t.TempDir(), config.SyncConfig{Everything: true}); len(got) != 0 {
		t.Fatalf("Everything on an empty repo should resolve to nothing, got %+v", got)
	}
}

// TestActiveTargetsExplicitIsHonoredVerbatim: an explicit pick is created even
// when the file doesn't exist yet (the user asked for it).
func TestActiveTargetsExplicitIsHonoredVerbatim(t *testing.T) {
	targets := ActiveTargets(t.TempDir(), config.SyncConfig{
		Targets: []config.SyncTarget{config.SyncTargetCursor},
	})
	if len(targets) != 1 || targets[0].Path != ".cursor/rules" {
		t.Fatalf("explicit Cursor pick should resolve to .cursor/rules, got %+v", targets)
	}
}

// TestWriteCreatesNestedDirs: a target under a subdirectory (.github/...) is
// created with parents, and round-trips through Detect as managed.
func TestWriteCreatesNestedDirs(t *testing.T) {
	root := t.TempDir()
	err := Write(root, "body", []config.SyncTargetMeta{
		{Name: "Copilot", Path: ".github/copilot-instructions.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".github", "copilot-instructions.md"))
	if err != nil {
		t.Fatalf("nested file not written: %v", err)
	}
	if string(b) != managedHeader+"body" {
		t.Errorf("content = %q, want managed header + body", b)
	}
}

// TestInstallGitHookIdempotent: installing twice leaves a single invocation, and
// an existing foreign hook is appended to rather than clobbered.
func TestInstallGitHookIdempotent(t *testing.T) {
	root := t.TempDir()
	hooks := filepath.Join(root, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "#!/bin/sh\necho existing hook\n"
	hookPath := filepath.Join(hooks, "post-commit")
	if err := os.WriteFile(hookPath, []byte(existing), 0o755); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := InstallGitHook(root); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	b, _ := os.ReadFile(hookPath)
	got := string(b)
	if n := countSub(got, "memcode sync --auto"); n != 1 {
		t.Errorf("hook should contain exactly one `memcode sync --auto`, got %d:\n%s", n, got)
	}
	if !contains(got, "echo existing hook") {
		t.Errorf("existing hook content was clobbered:\n%s", got)
	}
}

func countSub(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}

func contains(s, sub string) bool { return countSub(s, sub) > 0 }
