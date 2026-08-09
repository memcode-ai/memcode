//go:build darwin || freebsd || linux || netbsd || openbsd || zos

package vaxis

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDrainDiscardsPending locks the exit-leak fix: Drain must read and discard the terminal's
// queued query replies (e.g. the DA1 / geometry reports) so they don't spill into the user's shell
// after the tty is restored to cooked mode. We simulate the tty with a pipe pre-loaded with reply
// bytes; after Drain the buffer must be empty.
func TestDrainDiscardsPending(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	// Exactly the kind of bytes that were leaking: a DA1 response and window/text-area reports.
	if _, err := w.WriteString("\x1b[?62;c\x1b[4;735;1239t\x1b[8;49;177t"); err != nil {
		t.Fatal(err)
	}

	(&unixTTY{readFile: r}).Drain()

	// Nothing should remain queued.
	fd := int(r.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	defer unix.SetNonblock(fd, false)
	buf := make([]byte, 256)
	if n, _ := unix.Read(fd, buf); n > 0 {
		t.Errorf("Drain left %d bytes unconsumed: %q", n, buf[:n])
	}
}

// TestDrainEmptyReturnsFast guards against a regression where Drain blocks (or sleeps long) on an
// already-empty terminal — it should return quickly, not stall every exit.
func TestDrainEmptyReturnsFast(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	start := time.Now()
	(&unixTTY{readFile: r}).Drain()
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("Drain on an empty tty took %s — should be near-instant", d)
	}
}
