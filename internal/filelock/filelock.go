// Package filelock is a cross-process advisory lock on a file: flock(2) on
// unix, LockFileEx on Windows. The OS drops the lock when the holder exits or
// is killed, so a crashed process never leaves it stuck. Acquire waits, but
// only as long as its context allows: a hung holder costs a bounded wait and a
// clear error, never a frozen memcode.
//
// It exists for credential refresh: OAuth refresh tokens are single-use and
// rotate, so the read→refresh→write-back of a login must not interleave across
// processes (two memcode sessions, or memcode and the vendor's own CLI when it
// honors the same convention).
package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/memcode-ai/memcode/internal/statedir"
)

// pollInterval is how often a waiting Acquire retries a non-blocking lock.
// Non-blocking + poll (rather than a blocking syscall) is what makes the wait
// cancellable on every platform.
const pollInterval = 25 * time.Millisecond

// errWouldBlock is returned by the platform tryLock when another holder has it.
var errWouldBlock = errors.New("filelock: held by another process")

// Lock is a held lock. Release it exactly once (extra calls are no-ops).
type Lock struct {
	f *os.File
}

// Path is the lock file for a named resource under the memcode state home
// (statedir.Home()/locks/<name>.lock) — never inside another tool's directory.
func Path(name string) string {
	return filepath.Join(statedir.Home(), "locks", name+".lock")
}

// Acquire takes an exclusive lock on path, creating the file (and its
// directory) when missing. It blocks until the lock is free or ctx is done; a
// ctx expiry returns an error wrapping ctx.Err().
func Acquire(ctx context.Context, path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("filelock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filelock: %w", err)
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		err := tryLock(f)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, errWouldBlock) {
			f.Close()
			return nil, fmt.Errorf("filelock %s: %w", path, err)
		}
		t.Reset(pollInterval)
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("filelock: timed out waiting for %s (another process holds it): %w", path, ctx.Err())
		case <-t.C:
		}
	}
}

// AcquireNamed is Acquire on Path(name), bounded by timeout as well as ctx.
func AcquireNamed(ctx context.Context, name string, timeout time.Duration) (*Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return Acquire(ctx, Path(name))
}

// Release drops the lock. Closing the descriptor releases it too, which is
// also what the OS does when the process dies.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlock(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
