//go:build !unix

package vxui

// drainAvailable is a no-op on non-unix platforms: startup keystroke capture is unix-only for now
// (the non-blocking tty drain uses unix syscalls). The banner still renders; nothing is captured.
func drainAvailable(fd uintptr) []byte { return nil }
