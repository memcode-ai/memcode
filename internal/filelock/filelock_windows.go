//go:build windows

package filelock

import (
	"os"
	"syscall"
	"unsafe"
)

// LockFileEx/UnlockFileEx via kernel32 directly, so the package needs only the
// standard library.
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
	errorIOPending          = syscall.Errno(997)
)

func tryLock(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return nil
	}
	if err == errorLockViolation || err == errorIOPending {
		return errWouldBlock
	}
	return err
}

func unlock(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		return err
	}
	return nil
}
