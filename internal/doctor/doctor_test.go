package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/config"
)

func status(rs []Result, name string) Status {
	for _, r := range rs {
		if r.Name == name {
			return r.Status
		}
	}
	return -1
}

// Check flags a real problem (not initialized, .memcode not ignored) without
// MUTATING the repo, and clears the self-ignore check once the bare-`*` .gitignore
// is present — the case my first cut of the check got wrong (it probed the dir, not
// its contents, and would have false-failed exactly this setup).
func TestDoctorChecks(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	_ = os.WriteFile(filepath.Join(root, "app.go"), []byte("package x\n"), 0o644)

	rs := Check(context.Background(), nil, root, nil)
	if status(rs, "initialized") != Fail {
		t.Errorf("uninitialized repo should fail 'initialized'; got %v", status(rs, "initialized"))
	}
	if status(rs, ".memcode self-ignored") != Fail {
		t.Errorf("no self-ignore yet → should fail; got %v", status(rs, ".memcode self-ignored"))
	}
	// A diagnostic must not create state: the writability probe must not make .memcode.
	if _, err := os.Stat(filepath.Join(root, config.DirName)); !os.IsNotExist(err) {
		t.Error("doctor must not create .memcode (a health check shouldn't mutate state)")
	}

	// Add the bare-`*` self-ignore → the check must pass (probing a content path).
	md := filepath.Join(root, config.DirName)
	_ = os.MkdirAll(md, 0o755)
	_ = os.WriteFile(filepath.Join(md, ".gitignore"), []byte("*\n"), 0o644)
	rs = Check(context.Background(), nil, root, nil)
	if status(rs, ".memcode self-ignored") != OK {
		t.Errorf("bare-* self-ignore should PASS the check; got %v", status(rs, ".memcode self-ignored"))
	}

	if out := Render(rs); !strings.Contains(out, "fix") && !strings.Contains(out, "healthy") {
		t.Errorf("render should include a summary line, got:\n%s", out)
	}
}

// detectLanguages scans root + first-level subdirs by marker file (a monorepo's go.mod
// often lives one level down), never counting dependency trees as repo languages.
func TestDetectLanguages(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "cli"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "cli", "go.mod"), []byte("module x\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "web"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "web", "tsconfig.json"), []byte("{}"), 0o644)
	// A dependency tree's markers must NOT count.
	nm := filepath.Join(root, "node_modules", "pkg")
	_ = os.MkdirAll(nm, 0o755)
	_ = os.WriteFile(filepath.Join(root, "node_modules", "pkg", "go.mod"), []byte("module y\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "node_modules", "pyproject.toml"), []byte(""), 0o644)

	got := detectLanguages(root)
	want := []string{"go", "typescript"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("detectLanguages = %v, want %v", got, want)
	}

	if langs := detectLanguages(t.TempDir()); len(langs) != 0 {
		t.Errorf("markerless repo should detect nothing, got %v", langs)
	}
}

// The language-server check reports per detected language: OK when the binary is on
// PATH, Warn (never Fail — fallbacks still work) with an install hint when missing,
// and no rows at all for a markerless repo.
func TestDoctorLSPChecks(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644)

	orig := lookPath
	defer func() { lookPath = orig }()

	lookPath = func(string) (string, error) { return "/usr/bin/fake", nil }
	rs := Check(context.Background(), nil, root, nil)
	if status(rs, "lsp (go)") != OK {
		t.Errorf("gopls on PATH → OK, got %v", status(rs, "lsp (go)"))
	}

	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	rs = Check(context.Background(), nil, root, nil)
	if status(rs, "lsp (go)") != Warn {
		t.Errorf("gopls missing → Warn (never Fail), got %v", status(rs, "lsp (go)"))
	}
	for _, r := range rs {
		if r.Name == "lsp (go)" && !strings.Contains(r.Detail, "go install") {
			t.Errorf("missing-server detail must carry the install hint, got %q", r.Detail)
		}
	}

	// Markerless repo → no lsp rows at all (don't clutter the healthy path).
	rs = Check(context.Background(), nil, t.TempDir(), nil)
	if status(rs, "lsp (go)") != -1 || status(rs, "lsp (typescript)") != -1 || status(rs, "lsp (python)") != -1 {
		t.Error("repo with no language markers must emit no lsp rows")
	}
}
