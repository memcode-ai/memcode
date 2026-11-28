//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package filelock

import (
	"errors"
	"os"
	"syscall"
)

func tryLock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN):
			return errWouldBlock
		default:
			return err
		}
	}
}

func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
