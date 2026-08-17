package browserrender

import (
	"os"
	"path/filepath"
	"testing"
)

// CHROME_PATH is a PIN: when set but invalid, discovery must FAIL rather than
// silently using a different Chrome than the one the user named.
func TestChromePathPinIsStrict(t *testing.T) {
	t.Setenv("CHROME_PATH", "/definitely/not/a/browser")
	if p, ok := Find(); ok {
		t.Errorf("an invalid CHROME_PATH must fail discovery, got %q", p)
	}
	// A valid pin wins outright.
	f := filepath.Join(t.TempDir(), "chrome")
	if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHROME_PATH", f)
	if p, ok := Find(); !ok || p != f {
		t.Errorf("a valid CHROME_PATH must win, got %q ok=%v", p, ok)
	}
}
