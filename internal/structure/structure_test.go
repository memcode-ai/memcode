package structure

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/store"
)

// writeFiles creates a synthetic repo layout under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanMonorepoTopology(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// A pnpm + Go monorepo: root is a workspace aggregator (not a subsystem);
	// apps/web depends on packages/ui via a workspace dependency.
	writeFiles(t, root, map[string]string{
		"pnpm-workspace.yaml":            "packages:\n  - apps/*\n  - packages/*\n",
		"package.json":                   `{"name":"root","private":true,"workspaces":["apps/*","packages/*"]}`,
		"apps/web/package.json":          `{"name":"@acme/web","dependencies":{"@acme/ui":"workspace:*"}}`,
		"packages/ui/package.json":       `{"name":"@acme/ui"}`,
		"services/api/go.mod":            "module github.com/acme/api\n\ngo 1.26\n",
		"node_modules/junk/package.json": `{"name":"junk"}`, // must be skipped
	})

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	res, err := Scan(ctx, s, root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	got := map[string]string{} // key -> ecosystem
	for _, sub := range res.Subsystems {
		got[sub.Key] = sub.Ecosystem
	}
	want := map[string]string{
		"apps/web":     EcoNode,
		"packages/ui":  EcoNode,
		"services/api": EcoGo,
	}
	if len(got) != len(want) {
		t.Fatalf("subsystems = %v, want keys %v", got, want)
	}
	for k, eco := range want {
		if got[k] != eco {
			t.Errorf("subsystem %q ecosystem = %q, want %q", k, got[k], eco)
		}
	}
	if _, ok := got["."]; ok {
		t.Error("workspace root should not be a subsystem")
	}
	if _, ok := got["node_modules/junk"]; ok {
		t.Error("node_modules must be skipped")
	}

	// The workspace dependency must resolve to an intra-repo edge.
	if len(res.Deps) != 1 || res.Deps[0].From != "apps/web" || res.Deps[0].To != "packages/ui" {
		t.Fatalf("deps = %+v, want apps/web -> packages/ui", res.Deps)
	}

	// Topology must be persisted as the structural state.
	st, ok, err := s.GetState(ctx, "repo", "structural")
	if err != nil || !ok {
		t.Fatalf("structural state missing: ok=%v err=%v", ok, err)
	}
	if len(st.Body) == 0 {
		t.Error("structural state body is empty")
	}
}

func TestLoadReconstructsTopology(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"package.json":              `{"name":"root","private":true,"workspaces":["packages/*"]}`,
		"pnpm-workspace.yaml":       "packages:\n  - packages/*\n",
		"packages/app/package.json": `{"name":"@acme/app","dependencies":{"@acme/lib":"workspace:*"}}`,
		"packages/lib/package.json": `{"name":"@acme/lib"}`,
	})

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	scanned, err := Scan(ctx, s, root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	loaded, err := Load(ctx, s)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Subsystems) != len(scanned.Subsystems) {
		t.Fatalf("Load subsystems = %d, scan = %d", len(loaded.Subsystems), len(scanned.Subsystems))
	}
	if len(loaded.Deps) != 1 || loaded.Deps[0].From != "packages/app" || loaded.Deps[0].To != "packages/lib" {
		t.Fatalf("Load deps = %+v, want packages/app -> packages/lib", loaded.Deps)
	}
	// Subsystem attrs survive the round-trip.
	var found bool
	for _, sub := range loaded.Subsystems {
		if sub.Key == "packages/app" {
			found = true
			if sub.Package != "@acme/app" || sub.Ecosystem != EcoNode {
				t.Errorf("subsystem attrs lost: %+v", sub)
			}
		}
	}
	if !found {
		t.Error("packages/app not loaded")
	}
}

func TestScanSinglePackageRepo(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"go.mod":    "module github.com/acme/tool\n\ngo 1.26\n",
		"main.go":   "package main\nfunc main() {}\n",
		"README.md": "# tool\n",
	})

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	res, err := Scan(ctx, s, root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// Single-package repo: the root IS the one subsystem.
	if len(res.Subsystems) != 1 || res.Subsystems[0].Key != "." || res.Subsystems[0].Ecosystem != EcoGo {
		t.Fatalf("subsystems = %+v, want one go subsystem at '.'", res.Subsystems)
	}
}

// initGitRepo creates a real git repo with one commit at dir, so Scan's
// per-subsystem git stats have real history to find.
func initGitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	writeFiles(t, dir, files)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("add", ".")
	run("commit", "-q", "-m", "initial commit in "+filepath.Base(dir))
}

// TestScanAcrossSiblingReposUnderNonGitParent reproduces the demonstrated bug:
// two independent repos living under one non-git parent directory. Scanning
// the parent must attribute real commit history to each subsystem instead of
// silently returning zeros because the parent itself isn't a work tree.
func TestScanAcrossSiblingReposUnderNonGitParent(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()

	initGitRepo(t, filepath.Join(parent, "service-a"), map[string]string{
		"go.mod":  "module github.com/acme/service-a\n\ngo 1.26\n",
		"main.go": "package main\nfunc main() {}\n",
	})
	initGitRepo(t, filepath.Join(parent, "service-b"), map[string]string{
		"go.mod":  "module github.com/acme/service-b\n\ngo 1.26\n",
		"main.go": "package main\nfunc main() {}\n",
	})

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	res, err := Scan(ctx, s, parent)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Subsystems) != 2 {
		t.Fatalf("subsystems = %+v, want 2", res.Subsystems)
	}
	for _, sub := range res.Subsystems {
		if sub.Commits == 0 {
			t.Errorf("subsystem %q has 0 commits, want its own repo's history attributed", sub.Key)
		}
		if len(sub.Owners) == 0 {
			t.Errorf("subsystem %q has no owners, want the commit author attributed", sub.Key)
		}
	}
}
