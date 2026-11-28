//go:build unix

package sessionprune

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a running process. EPERM means it
// exists but belongs to someone else, which still counts.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
