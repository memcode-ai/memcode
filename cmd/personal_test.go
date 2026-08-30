package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

func TestPersonalCreateListShowPauseResumeStopDelete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	exec := func(args ...string) (string, error) {
		cmd := rootCmd
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	if _, err := exec("personal", "create", "test-agent", "Maintain an arbitrary outcome"); err != nil {
		t.Fatal(err)
	}
	cfg, err := gwconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents["test-agent"].Kind != "personal" {
		t.Fatalf("agent=%+v", cfg.Agents["test-agent"])
	}
	if _, err := os.Stat(filepath.Join(home, ".memcode", "agents", "test-agent", "personal.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := exec("personal", "pause", "test-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := exec("personal", "resume", "test-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := exec("personal", "stop", "test-agent"); err != nil {
		t.Fatal(err)
	}
	out, err := exec("personal", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "test-agent") {
		t.Fatalf("list output missing agent: %q", out)
	}
	if _, err := exec("personal", "delete", "test-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".memcode", "agents", "test-agent")); err != nil {
		t.Fatal("non-destructive delete removed home")
	}
}
