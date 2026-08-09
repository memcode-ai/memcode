//go:build !windows

package jobs

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts cmd in a NEW SESSION (Setsid), which does two things we need for a
// captured (non-interactive) command lane:
//  1. it's the session+group leader, so its pgid == pid and killGroup(-pid) reaps the whole
//     tree — a plain CommandContext kill would orphan npm's node child (see RunForeground);
//  2. it has NO controlling terminal, so a TUI/interactive child (vim, top, or memcode itself)
//     that opens /dev/tty fails fast ("device not configured") and exits, instead of grabbing
//     the parent's terminal and HANGING the lane. This matches how Claude Code runs `!`/bash.
//
// (Setsid alone creates the new process group; don't also set Setpgid — it'd be redundant and
// can fail once the child is already a group leader.)
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// killGroup signals the whole process group (negative pid) so a server and its
// children all die — a plain Process.Kill() would orphan npm's node child.
func killGroup(c *exec.Cmd) {
	if c == nil || c.Process == nil {
		return
	}
	_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	_ = c.Process.Kill() // belt-and-suspenders if Setpgid didn't take
}
