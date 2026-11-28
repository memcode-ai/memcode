package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The shield is OS-enforced on every agent command, not only in opt-in
// containment: memcode's credentials can't be read, memcode's authority files
// can't be written, other projects' state is invisible — while the repo, tmp
// and ordinary reads keep working.
func TestShieldEnforcesOnMac(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt is macOS-only")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec unavailable")
	}
	base := real(t.TempDir())
	home := filepath.Join(base, "home")
	state := filepath.Join(base, "state")
	repo := filepath.Join(base, "repo")
	proj := filepath.Join(state, "projects", "this")
	other := filepath.Join(state, "projects", "other")
	creds := filepath.Join(home, ".config", "memcode")
	for _, d := range []string{repo, filepath.Join(proj, "codeexec"), other, creds, filepath.Join(state, "websites")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(creds, ".env"), "MEMCODE_API_TOKEN=secret")
	write(filepath.Join(other, "transcript.md"), "other project's chat")
	write(filepath.Join(proj, "permissions"), "rule")
	write(filepath.Join(state, "hooks.json"), "{}")

	sh := &Shield{
		Root: repo, ProjectState: proj, StateHome: state,
		NoAccess: []string{creds, filepath.Join(state, "projects")},
		Writable: []string{filepath.Join(state, "websites")},
		Readable: []string{filepath.Join(proj, "codeexec")},
	}
	run := func(line string) (string, error) {
		wrapped, ok := Wrap(line, Policy{Mode: Off, Root: repo, Shield: sh})
		if !ok {
			t.Fatal("shield must engage in Off mode")
		}
		out, err := exec.Command("sh", "-c", wrapped).CombinedOutput()
		return string(out), err
	}
	denied := map[string]string{
		"read memcode credentials":   "cat " + shq(filepath.Join(creds, ".env")),
		"read another project":       "cat " + shq(filepath.Join(other, "transcript.md")),
		"approve its own commands":   "echo 'rm *\ttrusted' >> " + shq(filepath.Join(proj, "permissions")),
		"add a global hook":          "echo '{}' > " + shq(filepath.Join(state, "hooks.json")),
		"write into another project": "touch " + shq(filepath.Join(other, "x")),
	}
	for what, line := range denied {
		if out, err := run(line); err == nil {
			t.Errorf("shield let a command %s: %q", what, out)
		}
	}
	allowed := map[string]string{
		"write the repo":        "echo ok > " + shq(filepath.Join(repo, "f")) + " && cat " + shq(filepath.Join(repo, "f")),
		"write tmp":             `touch "${TMPDIR:-/tmp}/memcode-shield-ok"`,
		"use scratch":           "touch " + shq(filepath.Join(proj, "codeexec", "x")),
		"edit a local site":     "touch " + shq(filepath.Join(state, "websites", "x")),
		"read ordinary files":   "head -1 /etc/hosts",
		"run git":               "git --version",
		"list the creds parent": "ls " + shq(filepath.Join(home, ".config")),
	}
	for what, line := range allowed {
		if out, err := run(line); err != nil {
			t.Errorf("shield broke an ordinary command (%s): %v %q", what, err, out)
		}
	}
	if out, _ := run("cat " + shq(filepath.Join(creds, ".env"))); strings.Contains(out, "secret") {
		t.Fatal("credential value leaked through the shield")
	}
}

// Force-disabling the sandbox turns the shield off too: the one escape hatch.
func TestShieldOffWhenSandboxDisabled(t *testing.T) {
	t.Setenv(EnvVar, "0")
	if p := PolicyFor(false, t.TempDir()); p.Shield != nil {
		t.Fatal("MEMCODE_SANDBOX=0 must disable the shield")
	}
	t.Setenv(EnvVar, "")
	if p := PolicyFor(false, t.TempDir()); p.Shield == nil {
		t.Fatal("the shield must be on by default")
	}
}
