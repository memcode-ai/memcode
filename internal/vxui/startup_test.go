package vxui

import "testing"

func TestPrintableInput(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain typing", "hi ca", "hi ca"},
		{"typed enter becomes space, trimmed", "hello\r", "hello"},
		{"embedded newline joins", "hi\nthere", "hi there"},
		{"strips a stray arrow-key escape", "ab\x1b[Ccd", "abcd"},
		{"drops bare control bytes", "a\x01b\x07c", "abc"},
		{"unicode survives", "café", "café"},
		{"leading/trailing space trimmed", "  go  ", "go"},
		{"empty", "", ""},
		{"only controls → empty", "\x1b[A\x1b[B\r\n", ""},
	}
	for _, c := range cases {
		if got := printableInput([]byte(c.in)); got != c.want {
			t.Errorf("%s: printableInput(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
