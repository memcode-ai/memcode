// keyprobe is a tiny raw-stdin key inspector. It puts the terminal in raw mode
// and prints the EXACT bytes your terminal sends for each key, so we can see what
// (for example) Shift+Enter actually transmits in your environment instead of
// guessing. It walks you through the keys one at a time so each byte sequence is
// unambiguously labeled. Run it, follow the prompts, paste the output back:
//
//	go run ./tools/keyprobe            # default terminal behavior
//	go run ./tools/keyprobe -enhance   # with kitty + modifyOtherKeys enabled
//
// Press Ctrl+C at any prompt to abort.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/charmbracelet/x/term"
)

func main() {
	enhance := flag.Bool("enhance", false, "enable kitty + xterm modifyOtherKeys before probing")
	flag.Parse()

	fd := os.Stdin.Fd()
	state, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot set raw mode:", err)
		os.Exit(1)
	}
	defer term.Restore(fd, state)

	if *enhance {
		os.Stdout.WriteString("\x1b[>1u\x1b[>4;2m")      // kitty progressive enhancement + modifyOtherKeys lvl 2
		defer os.Stdout.WriteString("\x1b[<u\x1b[>4;0m") // pop kitty flags + reset modifyOtherKeys
	}

	mode := "default"
	if *enhance {
		mode = "ENHANCED"
	}
	fmt.Printf("keyprobe (%s mode). Press each key when prompted. Ctrl+C aborts.\r\n\r\n", mode)

	keys := []string{
		"ENTER (Return)",
		"SHIFT+ENTER",
		"CTRL+J",
		"OPTION+ENTER (Alt+Enter)",
	}
	buf := make([]byte, 64)
	for _, name := range keys {
		fmt.Printf("→ press %-26s ", name)
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			fmt.Print("\r\n(read error / EOF)\r\n")
			return
		}
		b := buf[:n]
		for _, c := range b {
			if c == 0x03 { // Ctrl+C
				fmt.Print("\r\n(aborted)\r\n")
				return
			}
		}
		fmt.Printf("→ %d byte(s)  hex=% x  caret=%s\r\n", n, b, caret(b))
	}
	fmt.Print("\r\ndone — paste this whole block back.\r\n")
}

// caret renders control bytes in caret notation (^M for CR, ^J for LF, ^[ for ESC).
func caret(b []byte) string {
	out := make([]rune, 0, len(b)*2)
	for _, c := range b {
		switch {
		case c == 0x1b:
			out = append(out, '^', '[')
		case c < 0x20:
			out = append(out, '^', rune(c+'@'))
		case c == 0x7f:
			out = append(out, '^', '?')
		default:
			out = append(out, rune(c))
		}
	}
	return string(out)
}
