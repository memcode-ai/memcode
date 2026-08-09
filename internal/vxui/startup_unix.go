//go:build unix

package vxui

import "syscall"

// drainAvailable reads every byte already waiting on the tty (keystrokes typed during the startup
// banner) without blocking: it flips the fd non-blocking, reads until the kernel buffer is empty
// (EAGAIN → n<=0 or error), and restores blocking mode. The caller holds the fd in raw mode, so the
// bytes are the literal characters the user typed, not a cooked line.
func drainAvailable(fd uintptr) []byte {
	ifd := int(fd)
	if err := syscall.SetNonblock(ifd, true); err != nil {
		return nil
	}
	defer syscall.SetNonblock(ifd, false)
	var out []byte
	buf := make([]byte, 1024)
	for {
		n, err := syscall.Read(ifd, buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if n <= 0 || err != nil {
			return out
		}
	}
}
