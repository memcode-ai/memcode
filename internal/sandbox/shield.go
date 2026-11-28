package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/statedir"
)

// Shield is the part of the sandbox that is on for every agent command, not
// only in opt-in containment. It is an OS-enforced boundary (Seatbelt on
// macOS, bubblewrap on Linux), unlike the output redaction and command
// classification layered above it, and it protects two things a prompt
// injection must never reach through a shell:
//
//   - credentials ordinary development never needs: memcode's own
//     (~/.config/memcode) and the subscription logins memcode can ride on
//     (Claude Code's and Codex's OAuth files) — unreadable;
//   - memcode's authority: everything under ~/.memcode is read-only except
//     site sources, skills, the current project's scratch area and the working
//     tree itself, so a command can't approve its own future commands, add a
//     hook or an MCP server, or rewrite memory; other projects' state
//     (transcripts, checkpoints) is unreadable.
//
// Everything else (the repo, ~/.ssh for git, cloud CLI configs for deploys,
// the network) stays as available as before: Auto mode keeps working for
// ordinary development.
type Shield struct {
	Root         string   // the working tree (may itself live under ~/.memcode, e.g. a task worktree)
	ProjectState string   // this project's state dir
	StateHome    string   // ~/.memcode (or MEMCODE_HOME)
	NoAccess     []string // directories neither readable nor writable
	NoAccessFile []string // single files neither readable nor writable
	Writable     []string // under StateHome, still writable
	Readable     []string // under StateHome/projects, still readable (and writable)
}

// ShieldFor builds the shield for a session rooted at root.
func ShieldFor(root string) *Shield {
	home, _ := os.UserHomeDir()
	state := statedir.Home()
	proj := statedir.For(root)
	sh := &Shield{Root: real(root), ProjectState: real(proj), StateHome: real(state)}
	if home != "" {
		sh.NoAccess = append(sh.NoAccess, real(filepath.Join(home, ".config", "memcode")))
		sh.NoAccessFile = append(sh.NoAccessFile,
			real(filepath.Join(home, ".claude", ".credentials.json")),
			real(filepath.Join(home, ".codex", "auth.json")),
		)
	}
	sh.NoAccess = append(sh.NoAccess, filepath.Join(sh.StateHome, "projects"))
	sh.Writable = []string{
		filepath.Join(sh.StateHome, "websites"),
		filepath.Join(sh.StateHome, "skills"),
	}
	sh.Readable = []string{
		filepath.Join(sh.ProjectState, "codeexec"),
		filepath.Join(sh.ProjectState, "skills"),
	}
	return sh
}

// real resolves symlinks on the longest existing prefix, so profiles name the
// paths the kernel actually checks (/var → /private/var on macOS).
func real(p string) string {
	tail := ""
	for dir := filepath.Clean(p); ; {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, tail)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		tail = filepath.Join(filepath.Base(dir), tail)
		dir = parent
	}
}

// seatbelt renders the shield as SBPL rules appended after the mode's own
// rules. Later rules win, so the order is: deny, then re-allow what stays
// usable, with the working tree last.
func (sh *Shield) seatbelt(mode Mode) string {
	if sh == nil {
		return ""
	}
	var b strings.Builder
	rule := func(verb, ops, kind string, paths ...string) {
		for _, p := range paths {
			if p != "" {
				fmt.Fprintf(&b, "\n(%s %s (%s %q))", verb, ops, kind, p)
			}
		}
	}
	rule("deny", "file-write*", "subpath", sh.StateHome)
	rule("deny", "file-read* file-write*", "subpath", sh.NoAccess...)
	rule("deny", "file-read* file-write*", "literal", sh.NoAccessFile...)
	rule("allow", "file-read*", "subpath", sh.Readable...)
	if mode == Off {
		rule("allow", "file-write*", "subpath", sh.Writable...)
		rule("allow", "file-write*", "subpath", sh.Readable...)
	}
	if sh.Root != "" {
		rule("allow", "file-read*", "subpath", sh.Root)
		if mode != ReadOnly {
			rule("allow", "file-write*", "subpath", sh.Root)
		}
	}
	return b.String()
}

// bwrap renders the shield as bubblewrap mount arguments, applied after the
// mode's own binds (later mounts cover earlier ones).
func (sh *Shield) bwrap(mode Mode) []string {
	if sh == nil {
		return nil
	}
	var args []string
	isDir := func(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if isDir(sh.StateHome) {
		args = append(args, "--ro-bind", shq(sh.StateHome), shq(sh.StateHome))
	}
	for _, d := range sh.NoAccess {
		if isDir(d) {
			args = append(args, "--tmpfs", shq(d))
		}
	}
	for _, f := range sh.NoAccessFile {
		if exists(f) {
			args = append(args, "--ro-bind", "/dev/null", shq(f))
		}
	}
	bind := "--ro-bind"
	if mode == Off {
		bind = "--bind"
	}
	for _, d := range sh.Readable {
		if isDir(d) {
			args = append(args, bind, shq(d), shq(d))
		}
	}
	if mode == Off {
		for _, d := range sh.Writable {
			if isDir(d) {
				args = append(args, "--bind", shq(d), shq(d))
			}
		}
	}
	if sh.Root != "" && isDir(sh.Root) {
		rb := "--bind"
		if mode == ReadOnly {
			rb = "--ro-bind"
		}
		args = append(args, rb, shq(sh.Root), shq(sh.Root))
	}
	return args
}

// NestingFailure reports output showing a command failed only because it ran
// inside memcode's sandbox: it tried to start its own sandbox (Seatbelt can't
// nest; bubblewrap may lack namespaces), or to gain privileges with sudo
// (setuid programs don't run sandboxed on macOS).
func NestingFailure(output string) bool {
	return strings.Contains(output, "sandbox_apply: Operation not permitted") ||
		strings.Contains(output, "bwrap: Creating new namespace failed") ||
		strings.Contains(output, "/sudo: Operation not permitted")
}

// ShieldOnly is the policy for a long-lived helper process memcode starts on
// the user's behalf (an MCP server): no write containment — it keeps its own
// caches and state — but the shield, unless sandboxing is force-disabled.
func ShieldOnly(root string) Policy {
	if PolicyFor(false, root).Shield == nil {
		return Policy{Mode: Off}
	}
	return Policy{Mode: Off, Root: root, Shield: ShieldFor(root)}
}

// WrapArgv is Wrap for a program and its arguments, quoted for the shell.
func WrapArgv(argv []string, p Policy) (string, bool) {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shq(a)
	}
	return Wrap("exec "+strings.Join(q, " "), p)
}
