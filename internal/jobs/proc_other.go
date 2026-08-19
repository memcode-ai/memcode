//go:build windows

package jobs

import (
	"os"
	"syscall"
)

// processAlive reports whether a process with pid is running. On Windows there's
// no waitpid(WNOHANG), so we fall back to the signal-0 probe (best-effort: a
// zombie/already-exited process may report alive until reaped — Windows handles
// process lifetime differently, so this is less of a concern than on Unix).
// Reaping of released/detached children is handled by the OS, not us.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// processStartSig has no ps-based start-time source on Windows; ok=false means the identity
// check is skipped and liveness alone decides (the pre-signature behavior).
func processStartSig(int) (string, bool) { return "", false }
