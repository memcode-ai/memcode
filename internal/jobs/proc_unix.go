//go:build !windows

package jobs

import (
	"os"
	"syscall"
)

// processAlive reports whether a process with pid is running. On Unix it uses
// waitpid (WNOHANG) first — this both checks liveness AND reaps zombies, so a
// killed child isn't mistakenly seen as alive (a zombie answers signal 0 with
// "yes I exist" even though it's dead). Falls back to signal 0 for processes we
// don't own (ECHILD — released/reparented/detached children, or a pid that was
// never our child).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	var status syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	if err == nil {
		// wpid == 0: still running. wpid == pid: exited (and now reaped).
		return wpid == 0
	}
	// ECHILD: not our child — signal 0 is the best portable probe we have left.
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
