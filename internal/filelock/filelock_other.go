//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

package filelock

import "os"

// No advisory file locking on this platform: callers still hold their
// in-process mutex, so only cross-process exclusion is lost.
func tryLock(*os.File) error { return nil }

func unlock(*os.File) error { return nil }
