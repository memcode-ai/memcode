//go:build windows

package jobs

import "os/exec"

// setProcessGroup is a no-op on Windows — there are no Unix process groups.
// (A Windows job-object tree kill can land if Windows becomes a first-class
// agent platform; today this exists so the binary cross-compiles.)
func setProcessGroup(cmd *exec.Cmd) {}

// killGroup kills the direct child only — no group semantics on Windows.
func killGroup(c *exec.Cmd) {
	if c == nil || c.Process == nil {
		return
	}
	_ = c.Process.Kill()
}
