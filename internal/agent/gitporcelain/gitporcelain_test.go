package gitporcelain

import "testing"

func TestUnquote(t *testing.T) {
	cases := []struct{ in, want string }{
		// core.quotePath=true: non-ASCII bytes come out as octal escapes.
		{`"caf\303\251.txt"`, "café.txt"},
		// Escaped tab, quote, backslash.
		{`"a\tb.txt"`, "a\tb.txt"},
		{`"say \"hi\".md"`, `say "hi".md`},
		{`"back\\slash"`, `back\slash`},
		// Unquoted paths pass through untouched.
		{"plain.go", "plain.go"},
		{"dir/file with space.txt", "dir/file with space.txt"},
		// Malformed quoting is returned as-is, never mangled.
		{`"unterminated`, `"unterminated`},
		{`"bad \q escape"`, `"bad \q escape"`},
		{`""`, ""},
		{`"`, `"`},
	}
	for _, c := range cases {
		if got := Unquote(c.in); got != c.want {
			t.Errorf("Unquote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
