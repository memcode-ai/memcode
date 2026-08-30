package autonomy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGeneratedWorkspaceCommitAndRollback(t *testing.T) {
	home := t.TempDir()
	root, err := InitializeGeneratedWorkspace(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact.txt"), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CommitGenerated(root, "v1"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	rev := strings.TrimSpace(string(out))
	if err := os.WriteFile(filepath.Join(root, "artifact.txt"), []byte("regression"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RollbackGenerated(root, rev); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "artifact.txt"))
	if string(b) != "v1" {
		t.Fatalf("content=%q", b)
	}
}
func TestRunnerScrubsEnvironmentStagesInputsAndLimitsOutput(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh unavailable")
	}
	result, err := RunGenerated(context.Background(), RunSpec{Executable: shell, AllowedExecutables: []string{shell}, Args: []string{"-c", "cat input.txt; printf %s \"$SECRET\"; printf 123456789"}, Inputs: map[string][]byte{"input.txt": []byte("input")}, Timeout: time.Second, MaxOutputBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Stdout, "SECRET") || len(result.Stdout) > 8 || !strings.HasPrefix(result.Stdout, "input") {
		t.Fatalf("stdout=%q", result.Stdout)
	}
}
func TestRunnerDeniesAuthorityExpansionAndFailsClosed(t *testing.T) {
	if _, err := RunGenerated(context.Background(), RunSpec{Executable: "sh", AllowedExecutables: []string{"python"}}); err == nil {
		t.Fatal("unallowed executable accepted")
	}
	if !SandboxAvailable() {
		if _, err := RunGenerated(context.Background(), RunSpec{Executable: "sh", AllowedExecutables: []string{"sh"}, RequireHardenedSandbox: true}); err == nil {
			t.Fatal("missing hardened sandbox did not fail closed")
		}
	}
}
