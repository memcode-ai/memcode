//go:build darwin || freebsd || linux || netbsd || openbsd || zos

package vaxis

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type unixTTY struct {
	readFile  *os.File
	writeFile *os.File
	state     *term.State
}

func openTTY(path string) (tty, error) {
	if path != "" {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		return &unixTTY{readFile: file, writeFile: file}, nil
	}
	file, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err == nil {
		return &unixTTY{readFile: file, writeFile: file}, nil
	}
	fallback, fallbackErr := openFallbackTTY()
	if fallbackErr == nil {
		return fallback, nil
	}
	return nil, err
}

func openFallbackTTY() (tty, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, os.ErrInvalid
	}
	readFile, err := dupFile(os.Stdin)
	if err != nil {
		return nil, err
	}
	var writeFile *os.File
	switch {
	case term.IsTerminal(int(os.Stdout.Fd())):
		writeFile, err = dupFile(os.Stdout)
	case term.IsTerminal(int(os.Stderr.Fd())):
		writeFile, err = dupFile(os.Stderr)
	default:
		err = os.ErrInvalid
	}
	if err != nil {
		_ = readFile.Close()
		return nil, err
	}
	return &unixTTY{readFile: readFile, writeFile: writeFile}, nil
}

func dupFile(file *os.File) (*os.File, error) {
	fd, err := unix.Dup(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}

func (t *unixTTY) Read(p []byte) (int, error) {
	return t.readFile.Read(p)
}

func (t *unixTTY) Write(p []byte) (int, error) {
	return t.writeFile.Write(p)
}

func (t *unixTTY) Fd() uintptr {
	return t.readFile.Fd()
}

func (t *unixTTY) SetRaw() error {
	state, err := term.MakeRaw(int(t.readFile.Fd()))
	if err != nil {
		return err
	}
	t.state = state
	return nil
}

func (t *unixTTY) Reset() error {
	if t.state == nil {
		return nil
	}
	return term.Restore(int(t.readFile.Fd()), t.state)
}

// Drain reads and discards any bytes the terminal still has queued for us — the asynchronous
// replies to queries we send during teardown (the DA1 wake in Suspend, plus any window/text-area
// geometry reports from a late resize still in flight). It MUST run after the input parser has
// stopped (so there is no second reader on the fd) and BEFORE the termios is restored to cooked
// mode: otherwise those replies arrive after we've handed the terminal back and spill into the
// user's shell as literal escape text (e.g. "8;49;177;735;1239t"). Non-blocking, so an empty
// buffer returns at once; a couple of short grace rounds catch a reply that's microseconds behind.
func (t *unixTTY) Drain() {
	fd := int(t.readFile.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		return
	}
	defer func() { _ = unix.SetNonblock(fd, false) }()
	buf := make([]byte, 4096)
	for round := 0; round < 3; round++ {
		drained := false
		for {
			n, err := unix.Read(fd, buf)
			if n > 0 {
				drained = true
				continue
			}
			_ = err // EAGAIN (buffer empty) or a benign teardown error → end this pass
			break
		}
		if !drained && round > 0 {
			return // stayed empty across a grace round → nothing more is coming
		}
		time.Sleep(3 * time.Millisecond) // let a just-sent reply land, then sweep again
	}
}

func (t *unixTTY) Size() (Resize, error) {
	ws, err := unix.IoctlGetWinsize(int(t.writeFile.Fd()), unix.TIOCGWINSZ)
	if err == nil {
		return Resize{
			Cols:   int(ws.Col),
			Rows:   int(ws.Row),
			XPixel: int(ws.Xpixel),
			YPixel: int(ws.Ypixel),
		}, nil
	}
	cols, rows, sizeErr := term.GetSize(int(t.writeFile.Fd()))
	if sizeErr != nil {
		return Resize{}, fmt.Errorf("%w; fallback size query failed: %v", err, sizeErr)
	}
	return Resize{Cols: cols, Rows: rows}, nil
}

func (t *unixTTY) StartInput(*Vaxis) error {
	return nil
}

func (t *unixTTY) StopInput() error {
	return nil
}

func (t *unixTTY) Close() error {
	err := t.readFile.Close()
	if t.writeFile != t.readFile {
		if writeErr := t.writeFile.Close(); err == nil {
			err = writeErr
		}
	}
	return err
}
