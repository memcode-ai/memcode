package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestCache(t *testing.T, c checkCache) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".memcode"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCache(c)
}

func TestReexecTarget(t *testing.T) {
	// Staged newer than the running build → re-exec.
	writeTestCache(t, checkCache{CheckedAt: time.Now(), Latest: "v99.0.0", Installed: "v99.0.0"})
	if _, ok := reexecTarget(); !ok {
		t.Fatal("staged newer install did not trigger re-exec")
	}
	// Staged equal/older → run as-is (also the loop terminator: after the
	// swap the running build IS the installed version).
	writeTestCache(t, checkCache{CheckedAt: time.Now(), Latest: "v0.0.1", Installed: "v0.0.1"})
	if _, ok := reexecTarget(); ok {
		t.Fatal("older staged install triggered re-exec")
	}
	// Nothing staged (check-only cache) → run as-is.
	writeTestCache(t, checkCache{CheckedAt: time.Now(), Latest: "v99.0.0"})
	if _, ok := reexecTarget(); ok {
		t.Fatal("check-only cache (nothing installed) triggered re-exec")
	}
}

func TestReexecStagedGuards(t *testing.T) {
	writeTestCache(t, checkCache{CheckedAt: time.Now(), Latest: "v99.0.0", Installed: "v99.0.0"})
	called := false
	orig := syscallExec
	syscallExec = func(string, []string, []string) error { called = true; return nil }
	defer func() { syscallExec = orig }()

	t.Setenv("MEMCODE_REEXEC", "1")
	ReexecStaged()
	if called {
		t.Fatal("re-exec guard env did not prevent a second swap")
	}
}
