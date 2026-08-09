package vxui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/term"
)

// captureStartupInput runs the (possibly slow) startup banner with the terminal in RAW mode, so
// keystrokes typed before vaxis owns the tty are neither echoed as raw-escape garbage nor swallowed
// by cooked-mode line buffering. It then drains whatever was typed and returns it as printable text
// to SEED the composer — the user's first words, replayed — and restores the pristine termios so
// vaxis can take the tty cleanly (it saves/restores the original itself). `during` is called with
// raw=true so the banner can emit CRLF line endings (cooked NL→CRNL translation is off in raw mode).
// On a non-terminal stdin (pipe/test) it just runs `during` cooked and captures nothing.
func captureStartupInput(fd uintptr, during func(raw bool)) string {
	if !term.IsTerminal(fd) {
		during(false)
		return ""
	}
	orig, err := term.MakeRaw(fd)
	if err != nil {
		during(false)
		return ""
	}
	during(true)
	buf := drainAvailable(fd)
	_ = term.Restore(fd, orig) // pristine again before vaxis re-raws and saves its own original
	return printableInput(buf)
}

// printableInput extracts the human-typed text from a raw byte burst: it skips ESC-led control
// sequences (a stray arrow key during startup) and non-printing bytes, turns a typed Enter/Tab into
// a space (seed the text — never auto-submit a half-formed line), and keeps the literal characters.
func printableInput(b []byte) string {
	var sb strings.Builder
	rs := []rune(string(b))
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == 0x1b: // ESC — skip a CSI escape sequence up to its final byte (0x40–0x7e)
			if i+1 < len(rs) && rs[i+1] == '[' {
				i += 2
				for i < len(rs) && !(rs[i] >= 0x40 && rs[i] <= 0x7e) {
					i++
				}
			}
		case r == '\r' || r == '\n' || r == '\t':
			sb.WriteByte(' ')
		case unicode.IsPrint(r):
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}
