// Package sandbox wraps agent shell commands in an OS-level containment layer
// — defense-in-depth UNDER the permission classifier, not a replacement for
// it. The classifier decides whether a command may run; the sandbox bounds
// what a permitted command can touch if it (or something it spawns) misbehaves.
//
// Modes:
//
//	ReadOnly   — read-only sessions (plan/scout shells): the filesystem is
//	             readable but only tmp + tool caches are writable. Always on
//	             for read-only sessions when the platform supports it.
//	Workspace  — writes confined to the repo root + tmp + tool caches.
//	             Opt-in for normal sessions via MEMCODE_SANDBOX=1.
//
// Backends: macOS sandbox-exec (Seatbelt), Linux bubblewrap (bwrap) when
// installed. No backend → commands run unwrapped, exactly as before (fail
// open: the sandbox strengthens an existing gate, it must never brick a
// platform). MEMCODE_SANDBOX=0 force-disables wrapping everywhere.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Mode is the containment level.
type Mode int

const (
	Off Mode = iota
	ReadOnly
	Workspace
)

// Policy is what a wrapped command may touch.
type Policy struct {
	Mode Mode
	Root string // repo root (Workspace mode's writable tree)
	// DenyNetwork additionally cuts ALL network egress (Seatbelt deny network*,
	// bwrap --unshare-net). Used by mcp_code_exec: with writes confined AND no
	// network, generated code loses its exfiltration channel and can gate at
	// Medium instead of Dangerous. Bash keeps network (builds/tests need it).
	DenyNetwork bool
	// Shield keeps memcode's own credentials and authority out of the command's
	// reach, in every mode including Off: see shield.go. Set by PolicyFor unless
	// sandboxing is force-disabled.
	Shield *Shield
}

// EnvVar toggles sandboxing for NORMAL sessions: "1"/"true"/"workspace" wraps
// every bash call in Workspace mode; "0"/"off" force-disables all wrapping
// (including the read-only default) as the escape hatch.
const EnvVar = "MEMCODE_SANDBOX"

// PolicyFor decides the policy for a bash call: read-only sessions always get
// ReadOnly containment; normal sessions get Workspace only when opted in.
func PolicyFor(readOnly bool, root string) Policy {
	switch strings.ToLower(os.Getenv(EnvVar)) {
	case "0", "off", "false":
		return Policy{Mode: Off}
	case "1", "true", "workspace":
		if !readOnly {
			return Policy{Mode: Workspace, Root: root, Shield: ShieldFor(root)}
		}
	}
	if readOnly {
		return Policy{Mode: ReadOnly, Root: root, Shield: ShieldFor(root)}
	}
	return Policy{Mode: Off, Root: root, Shield: ShieldFor(root)}
}

// Supported reports whether Wrap would ACTUALLY contain under p on this
// platform — callers that scale a permission gate to the containment (mcp_code_exec:
// sandboxed+no-network = Medium, unsandboxed = Dangerous) must know the
// difference between "asked for a sandbox" and "got one".
func Supported(p Policy) bool {
	if p.Mode == Off && p.Shield == nil {
		return false
	}
	switch runtime.GOOS {
	case "darwin":
		_, err := os.Stat("/usr/bin/sandbox-exec")
		return err == nil
	case "linux":
		return bwrapWorks()
	}
	return false
}

var (
	bwrapOnce sync.Once
	bwrapOK   bool
)

// bwrapWorks reports whether bubblewrap can actually create a sandbox here.
// Installed is not enough: containers and CI runners often forbid the user
// namespaces it needs, and wrapping every command in a bwrap that always
// fails would break every command. Probed once per process.
func bwrapWorks() bool {
	bwrapOnce.Do(func() {
		bin, err := exec.LookPath("bwrap")
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bwrapOK = exec.CommandContext(ctx, bin, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "true").Run() == nil
	})
	return bwrapOK
}

// Wrap returns a shell line that runs command under the policy's containment,
// and whether wrapping applies. ok=false means run the command unchanged
// (policy Off, or no backend on this platform).
func Wrap(command string, p Policy) (string, bool) {
	if (p.Mode == Off && p.Shield == nil) || strings.TrimSpace(command) == "" {
		return command, false
	}
	switch runtime.GOOS {
	case "darwin":
		if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
			return command, false
		}
		return "/usr/bin/sandbox-exec -p " + shq(seatbeltProfile(p)) + " /bin/sh -c " + shq(command), true
	case "linux":
		bwrap, err := exec.LookPath("bwrap")
		if err != nil || !bwrapWorks() {
			return command, false
		}
		return bwrapLine(bwrap, command, p), true
	default:
		return command, false
	}
}

// writableExtras are always-writable trees beyond the policy root: temp dirs
// and user tool caches (go build, npm, …) — cache writes can't hurt the repo,
// and denying them breaks ordinary read commands like `go list`.
func writableExtras() []string {
	paths := []string{"/tmp", "/private/tmp", "/dev"}
	if runtime.GOOS == "darwin" {
		paths = append(paths, "/private/var/folders") // macOS per-user TMPDIR lives here
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".cache"))
		if runtime.GOOS == "darwin" {
			paths = append(paths, filepath.Join(home, "Library", "Caches"))
		}
	}
	return paths
}

// seatbeltProfile builds the macOS sandbox profile: allow everything, then
// deny writes, then re-allow the writable set.
func seatbeltProfile(p Policy) string {
	var allow []string
	for _, path := range writableExtras() {
		allow = append(allow, fmt.Sprintf("(subpath %q)", path))
	}
	if p.Mode == Workspace && p.Root != "" {
		root := p.Root
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		allow = append(allow, fmt.Sprintf("(subpath %q)", root))
	}
	profile := "(version 1)\n(allow default)"
	if p.Mode != Off {
		profile += "\n(deny file-write*)\n(allow file-write* " + strings.Join(allow, " ") + ")"
	}
	profile += p.Shield.seatbelt(p.Mode)
	if p.DenyNetwork {
		// Last rule wins in SBPL: this overrides the (allow default) for every
		// network operation. Inherited pipes are unaffected (they're fds, not
		// sockets) — the mcp_code_exec bridge keeps working.
		profile += "\n(deny network*)"
	}
	return profile
}

// bwrapLine builds the Linux bubblewrap invocation: read-only root bind, tmp
// and caches (and the workspace, per policy) bound writable.
func bwrapLine(bwrap, command string, p Policy) string {
	rootBind := "--ro-bind"
	if p.Mode == Off {
		rootBind = "--bind" // shield only: everything stays writable except what the shield protects
	}
	args := []string{shq(bwrap), rootBind, "/", "/", "--dev", "/dev", "--proc", "/proc", "--die-with-parent"}
	if p.DenyNetwork {
		args = append(args, "--unshare-net") // isolated net namespace: loopback only, no egress
	}
	bindIfExists := func(path string) {
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			args = append(args, "--bind", shq(path), shq(path))
		}
	}
	bindIfExists("/tmp")
	if home, err := os.UserHomeDir(); err == nil {
		bindIfExists(filepath.Join(home, ".cache"))
	}
	if p.Mode == Workspace && p.Root != "" {
		root := p.Root
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		bindIfExists(root)
	}
	args = append(args, p.Shield.bwrap(p.Mode)...)
	args = append(args, "/bin/sh", "-c", shq(command))
	return strings.Join(args, " ")
}

// shq single-quotes s for POSIX shells (” + the '\” dance).
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
