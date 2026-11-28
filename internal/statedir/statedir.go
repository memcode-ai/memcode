// Package statedir decides where memcode keeps a project's state. memcode does
// not write into the user's folders: everything it records about a project
// (sessions, the index, approvals, jobs, checkpoints, worktrees) lives in the
// user-global home, one directory per project, keyed by the project's path:
//
//	~/.memcode/projects/-Users-me-code-app/
//
// The one thing that still lives in the project is <root>/.memcode/tasks: an
// autonomous task is a standing instruction a team reviews and commits like
// code, so it belongs with the code (see internal/task).
//
// Leaf package on purpose (stdlib only): everything that touches project state
// imports it, and config imports half the tree.
package statedir

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// LegacyName is the per-project directory memcode used to create inside the
// project. Only tasks/ remains there; anything else is migrated out by Migrate.
const LegacyName = ".memcode"

// EnvHome overrides the user-global memcode home (default ~/.memcode).
const EnvHome = "MEMCODE_HOME"

var (
	testHomeOnce sync.Once
	testHome     string
	// startHome is $HOME when the process started, before any test redirected it.
	startHome = os.Getenv("HOME")
)

// Home is the user-global memcode directory: $MEMCODE_HOME, else ~/.memcode.
// Under `go test` it never resolves to the developer's real home: a test that
// redirected HOME gets $HOME/.memcode, any other test a per-process temp dir.
func Home() string {
	if h := os.Getenv(EnvHome); h != "" {
		return h
	}
	if testing.Testing() && os.Getenv("HOME") == startHome {
		testHomeOnce.Do(func() {
			d, err := os.MkdirTemp("", "memcode-test-home-")
			if err != nil {
				d = filepath.Join(os.TempDir(), "memcode-test-home")
			}
			testHome = d
		})
		return testHome
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), LegacyName)
	}
	return filepath.Join(home, LegacyName)
}

// Key turns a project root into its directory name under projects/: the
// absolute, cleaned path with separators (and a drive colon) replaced by '-',
// so it stays readable ("-Users-me-code-app").
func Key(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	abs = realPath(filepath.Clean(abs))
	r := strings.NewReplacer(string(filepath.Separator), "-", "/", "-", ":", "-")
	return r.Replace(abs)
}

var migrated sync.Map // Key(root) -> struct{}: Migrate has run for it in this process

// realPath resolves symlinks on the longest existing prefix of p and re-appends
// the rest, so a project has one key whether or not its folder exists yet
// (/tmp/x and /private/tmp/x are one project, before and after mkdir).
func realPath(p string) string {
	tail := ""
	for dir := p; ; {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		tail = filepath.Join(filepath.Base(dir), tail)
		dir = parent
	}
}

// For returns the state directory for the project at root. It is not created;
// writers MkdirAll as they always have. The first lookup for a root in a process
// silently moves any state an older memcode left in the project (Migrate), so
// every reader and writer sees the global directory, whatever the entry point.
func For(root string) string {
	key := Key(root)
	dir := filepath.Join(Home(), "projects", key)
	if _, done := migrated.LoadOrStore(key, struct{}{}); !done {
		migrate(root, dir)
	}
	return dir
}

// Migrate moves state an older memcode left inside the project
// (<root>/.memcode/*) into For(root), once per process (For does this itself). tasks/ stays in the project. It is
// idempotent and best-effort: a failure leaves the old directory in place and
// memcode simply starts fresh state in the global directory. Nothing is
// deleted unless it was moved first.
func Migrate(root string) { _ = For(root) }

func migrate(root, dst string) {
	legacy := filepath.Join(root, LegacyName)
	entries, err := os.ReadDir(legacy)
	if err != nil {
		return // nothing in the project — the normal case
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return
	}
	tracked := trackedUnder(root, LegacyName)
	for _, e := range entries {
		name := e.Name()
		if name == "tasks" || name == ".gitignore" || tracked[name] {
			// tasks/ belongs with the code; anything the repo itself tracks is the
			// repo's content, not memcode state: never move it (that would show up
			// as deletions, and would turn repo-supplied files into trusted state).
			continue
		}
		from, to := filepath.Join(legacy, name), filepath.Join(dst, name)
		if _, err := os.Lstat(to); err == nil {
			continue // already present globally; never overwrite
		}
		if name == "worktrees" {
			// Task worktrees are registered with git by path: move them with a plain
			// rename only (never copy), then tell git where they went.
			if os.Rename(from, to) == nil {
				repairWorktrees(root, to)
			}
			continue
		}
		if err := os.Rename(from, to); err != nil {
			// Different volume: copy, then remove the original only once copied.
			if err := copyTree(from, to); err == nil {
				_ = os.RemoveAll(from)
			}
		}
	}
	// Leave the project as we found it: if only the self-ignore (or nothing)
	// remains, the directory is gone; tasks/ keeps it alive.
	rest, err := os.ReadDir(legacy)
	if err != nil {
		return
	}
	for _, e := range rest {
		if e.Name() != ".gitignore" {
			return
		}
	}
	_ = os.RemoveAll(legacy)
}

func copyTree(from, to string) error {
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		return os.WriteFile(to, data, info.Mode().Perm())
	}
	return os.CopyFS(to, os.DirFS(from))
}

// repairWorktrees points git at task worktrees that Migrate moved, so `git
// worktree list` and the worktrees themselves keep working.
func repairWorktrees(root, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	args := []string{"-C", root, "worktree", "repair"}
	for _, e := range entries {
		if e.IsDir() {
			args = append(args, filepath.Join(dir, e.Name()))
		}
	}
	if len(args) > 4 {
		_ = exec.Command("git", args...).Run()
	}
}

// trackedUnder returns the top-level names under root/dir that git tracks. Not
// a git repo, or git missing: nothing is tracked.
func trackedUnder(root, dir string) map[string]bool {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", dir).Output()
	names := map[string]bool{}
	if err != nil {
		return names
	}
	for _, p := range strings.Split(string(out), "\x00") {
		rel := strings.TrimPrefix(filepath.ToSlash(p), dir+"/")
		if rel == "" || rel == p {
			continue
		}
		names[strings.SplitN(rel, "/", 2)[0]] = true
	}
	return names
}
