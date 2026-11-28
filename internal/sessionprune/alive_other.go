//go:build !unix

package sessionprune

import "os"

// processAlive reports whether pid names a running process. On Windows
// FindProcess opens a handle and fails for a process that has exited; anywhere
// else it errs on the side of "alive", which only ever refuses a delete.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
