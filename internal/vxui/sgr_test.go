package vxui

import "testing"

func TestNormalizeSGR(t *testing.T) {
	cases := map[string]string{
		// lipgloss truecolor foreground (semicolon) → vaxis colon form.
		"\x1b[38;2;255;128;0mhi\x1b[0m": "\x1b[38:2:255:128:0mhi\x1b[0m",
		// 256-color, with a leading attribute that must stay semicolon-separated.
		"\x1b[1;38;5;240mx\x1b[0m": "\x1b[1;38:5:240mx\x1b[0m",
		// background + foreground truecolor in one sequence.
		"\x1b[48;2;0;0;0;38;2;255;255;255m": "\x1b[48:2:0:0:0;38:2:255:255:255m",
		// pure attributes pass through untouched.
		"\x1b[1mbold\x1b[22m": "\x1b[1mbold\x1b[22m",
		// basic 16-color (single param) untouched.
		"\x1b[31mred\x1b[39m": "\x1b[31mred\x1b[39m",
		// no escapes at all.
		"plain text": "plain text",
	}
	for in, want := range cases {
		if got := NormalizeSGR(in); got != want {
			t.Errorf("NormalizeSGR(%q)\n  = %q\n want %q", in, got, want)
		}
	}
}
