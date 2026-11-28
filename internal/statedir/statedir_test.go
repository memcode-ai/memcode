package statedir

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A test never resolves to the developer's real home.
func TestHomeIsNotTheRealHomeUnderTest(t *testing.T) {
	if real, err := os.UserHomeDir(); err == nil && strings.HasPrefix(Home(), filepath.Join(real, LegacyName)) {
		t.Fatalf("Home() = %s, the real ~/.memcode, under go test", Home())
	}
	t.Setenv(EnvHome, "/x/home")
	if Home() != "/x/home" {
		t.Fatalf("MEMCODE_HOME not honored: %s", Home())
	}
}

// One project, one key: before and after its folder exists, and through a symlink.
func TestKeyStableAcrossCreationAndSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	before := Key(root)
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if after := Key(root); after != before {
		t.Fatalf("key changed when the folder was created: %q -> %q", before, after)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if Key(link) != before {
		t.Fatalf("symlinked path got its own key: %q vs %q", Key(link), before)
	}
	if strings.ContainsRune(before, filepath.Separator) {
		t.Fatalf("key %q still holds separators", before)
	}
}

// Old in-project state moves to the global directory silently; tasks/ stays in
// the project, nothing already global is overwritten, and an emptied .memcode
// disappears.
func TestMigrateMovesStateKeepsTasks(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	root := t.TempDir()
	legacy := filepath.Join(root, LegacyName)
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(legacy, "state.db"), "db")
	write(filepath.Join(legacy, "sessions", "s1", "events.jsonl"), "{}")
	write(filepath.Join(legacy, "tasks", "nightly.yaml"), "task")
	write(filepath.Join(legacy, ".gitignore"), "*\n")

	dir := For(root)
	if b, err := os.ReadFile(filepath.Join(dir, "sessions", "s1", "events.jsonl")); err != nil || string(b) != "{}" {
		t.Fatalf("sessions not migrated: %v %q", err, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		t.Fatalf("state.db not migrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "tasks", "nightly.yaml")); err != nil {
		t.Fatalf("tasks/ must stay in the project: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "state.db")); !os.IsNotExist(err) {
		t.Fatalf("state.db left behind in the project: %v", err)
	}

	// A project with nothing but state ends with no .memcode at all.
	other := t.TempDir()
	write(filepath.Join(other, LegacyName, "state.db"), "db")
	write(filepath.Join(other, LegacyName, ".gitignore"), "*\n")
	For(other)
	if _, err := os.Stat(filepath.Join(other, LegacyName)); !os.IsNotExist(err) {
		t.Fatalf("emptied .memcode should be gone: %v", err)
	}
	if fi, err := os.Stat(Home()); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state home must be private (0700): %v %v", err, fi.Mode())
	}
}

// Outside git, nothing says a .memcode came from memcode rather than from a
// downloaded archive, so files that grant capabilities (approved commands,
// hooks, MCP servers, endpoints) are never promoted into trusted state.
func TestMigrateOutsideGitLeavesCapabilityFiles(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	root := t.TempDir()
	for _, name := range []string{"permissions", "hooks.json", "mcp.json", "config.json", "state.db"} {
		p := filepath.Join(root, LegacyName, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := For(root)
	for _, name := range []string{"permissions", "hooks.json", "mcp.json", "config.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s from a non-git folder became trusted state", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		t.Fatalf("plain state should still move: %v", err)
	}
}

// Global state that already exists is never overwritten by an old copy.
func TestMigrateNeverOverwrites(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	root := t.TempDir()
	dst := filepath.Join(Home(), "projects", Key(root))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "config.json"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, LegacyName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, LegacyName, "config.json"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	For(root)
	if b, _ := os.ReadFile(filepath.Join(dst, "config.json")); string(b) != "new" {
		t.Fatalf("global config overwritten: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, LegacyName, "config.json")); string(b) != "old" {
		t.Fatalf("unmoved old copy must be left in place, got %q", b)
	}
}

// Files the repo itself tracks under .memcode are the repo's content: never moved.
func TestMigrateLeavesTrackedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv(EnvHome, t.TempDir())
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.MkdirAll(filepath.Join(root, LegacyName), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"hooks.json": "{}", "state.db": "db"} {
		if err := os.WriteFile(filepath.Join(root, LegacyName, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", filepath.Join(LegacyName, "hooks.json"))
	run("commit", "-qm", "repo ships a hooks file")

	dir := For(root)
	if _, err := os.Stat(filepath.Join(root, LegacyName, "hooks.json")); err != nil {
		t.Fatalf("tracked file was moved out of the repo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("tracked repo file must not become global state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		t.Fatalf("untracked state was not migrated: %v", err)
	}
}
