package provider

import (
	"os"
	"path/filepath"
	"testing"
)

// Lookup reads the saved login without putting it in the environment, so a
// client that needs the token can't make the rest of the process look signed in.
func TestLookupLeavesEnvironmentAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(EnvAPIToken, "")
	os.Unsetenv(EnvAPIToken)
	if err := os.MkdirAll(filepath.Join(dir, "memcode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memcode", ".env"), []byte(EnvAPIToken+"=\"saved\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Lookup(EnvAPIToken); got != "saved" {
		t.Fatalf("Lookup = %q, want the saved token", got)
	}
	if _, set := os.LookupEnv(EnvAPIToken); set {
		t.Fatal("Lookup put the token in the environment")
	}
	t.Setenv(EnvAPIToken, "exported")
	if got := Lookup(EnvAPIToken); got != "exported" {
		t.Fatalf("Lookup = %q, want the environment to win", got)
	}
}
