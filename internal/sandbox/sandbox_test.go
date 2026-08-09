package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPolicyFor(t *testing.T) {
	t.Setenv(EnvVar, "")
	if p := PolicyFor(true, "/r"); p.Mode != ReadOnly {
		t.Fatalf("read-only session must default to ReadOnly, got %v", p.Mode)
	}
	if p := PolicyFor(false, "/r"); p.Mode != Off {
		t.Fatalf("normal session must default to Off, got %v", p.Mode)
	}
	t.Setenv(EnvVar, "1")
	if p := PolicyFor(false, "/r"); p.Mode != Workspace || p.Root != "/r" {
		t.Fatalf("MEMCODE_SANDBOX=1 must yield Workspace, got %+v", p)
	}
	t.Setenv(EnvVar, "0")
	if p := PolicyFor(true, "/r"); p.Mode != Off {
		t.Fatalf("MEMCODE_SANDBOX=0 is the escape hatch, got %v", p.Mode)
	}
}

func TestShq(t *testing.T) {
	if got := shq("a'b"); got != `'a'\''b'` {
		t.Fatalf("shq = %s", got)
	}
	out, err := exec.Command("sh", "-c", "printf %s "+shq(`x'"$HOME y`)).Output()
	if runtime.GOOS == "windows" {
		t.Skip("sh quoting check is unix-only")
	}
	if err != nil || string(out) != `x'"$HOME y` {
		t.Fatalf("round-trip through sh failed: %q %v", out, err)
	}
}

func TestSeatbeltProfileShape(t *testing.T) {
	p := seatbeltProfile(Policy{Mode: Workspace, Root: "/repo/root"})
	for _, want := range []string{"(version 1)", "(deny file-write*)", `"/tmp"`, `"/repo/root"`} {
		if !strings.Contains(p, want) {
			t.Fatalf("profile missing %s:\n%s", want, p)
		}
	}
	ro := seatbeltProfile(Policy{Mode: ReadOnly, Root: "/repo/root"})
	if strings.Contains(ro, `"/repo/root"`) {
		t.Fatal("ReadOnly profile must NOT allow writes to the repo root")
	}
}

func TestWrapOffAndUnsupported(t *testing.T) {
	if got, ok := Wrap("echo hi", Policy{Mode: Off}); ok || got != "echo hi" {
		t.Fatalf("Off must not wrap: %q %v", got, ok)
	}
}

// TestSeatbeltEnforces actually runs wrapped commands on macOS. The probe
// target lives under $HOME (writable normally, NOT in the sandbox allow set —
// t.TempDir sits under /var/folders, which IS allowed as TMPDIR, so it can't
// discriminate): ReadOnly must deny it, Workspace with Root pointed at it must
// allow it, and tmp stays writable throughout.
func TestSeatbeltEnforces(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("seatbelt is macOS-only")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec unavailable")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	repo, err := os.MkdirTemp(home, ".memcode-sbx-test-*")
	if err != nil {
		t.Skipf("cannot create probe dir in home: %v", err)
	}
	defer os.RemoveAll(repo)
	probe := filepath.Join(repo, "probe.txt")

	run := func(line string) error {
		cmd := exec.Command("sh", "-c", line)
		done := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			return err
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatal("sandboxed command hung")
			return nil
		}
	}

	// Sanity: the probe IS writable without a sandbox.
	if err := run("touch " + shq(probe)); err != nil {
		t.Fatalf("unsandboxed control write failed: %v", err)
	}
	_ = os.Remove(probe)

	// ReadOnly: home probe denied, tmp allowed.
	if err := run(mustWrap(t, "touch "+shq(probe), Policy{Mode: ReadOnly, Root: repo})); err == nil {
		t.Fatal("ReadOnly must deny writes outside tmp/caches")
	}
	if err := run(mustWrap(t, `touch "${TMPDIR:-/tmp}/memcode-sbx-ok"`, Policy{Mode: ReadOnly})); err != nil {
		t.Fatalf("tmp write must stay allowed in ReadOnly: %v", err)
	}

	// Workspace rooted at the probe dir: the same write now succeeds.
	if err := run(mustWrap(t, "touch "+shq(probe), Policy{Mode: Workspace, Root: repo})); err != nil {
		t.Fatalf("Workspace must allow writes under its root: %v", err)
	}
}

func mustWrap(t *testing.T, cmd string, p Policy) string {
	t.Helper()
	line, ok := Wrap(cmd, p)
	if !ok {
		t.Fatalf("expected wrapping to engage for %q", cmd)
	}
	return line
}

// Network denial: the profile lines the mcp_code_exec Medium gate rests on.
func TestDenyNetworkProfiles(t *testing.T) {
	on := Policy{Mode: Workspace, Root: "/tmp/x", DenyNetwork: true}
	off := Policy{Mode: Workspace, Root: "/tmp/x"}
	if p := seatbeltProfile(on); !strings.Contains(p, "(deny network*)") {
		t.Errorf("seatbelt profile missing network denial:\n%s", p)
	}
	if p := seatbeltProfile(off); strings.Contains(p, "deny network") {
		t.Errorf("bash policy must keep network:\n%s", p)
	}
	if l := bwrapLine("bwrap", "true", on); !strings.Contains(l, "--unshare-net") {
		t.Errorf("bwrap line missing --unshare-net:\n%s", l)
	}
	if l := bwrapLine("bwrap", "true", off); strings.Contains(l, "--unshare-net") {
		t.Errorf("bash bwrap line must keep network:\n%s", l)
	}
}
