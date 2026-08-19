//go:build !windows

package update

import "syscall"

// syscallExec is a seam for tests; on unix it is the real process image swap.
var syscallExec = func(argv0 string, argv, envv []string) error {
	return syscall.Exec(argv0, argv, envv)
}
