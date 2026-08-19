package textutil

import (
	"testing"
	"unicode/utf8"
)

func TestClipRunes(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"héllo", 2, "hé"},
		{"café☕", 4, "café"},
		{"", 5, ""},
		{"abc", 0, ""},
		{"abc", -1, ""},
	}
	for _, c := range cases {
		if got := ClipRunes(c.in, c.max); got != c.want {
			t.Errorf("ClipRunes(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestClipRunesEllipsis(t *testing.T) {
	if got := ClipRunesEllipsis("hello", 3); got != "hel…" {
		t.Errorf("got %q", got)
	}
	if got := ClipRunesEllipsis("hi", 3); got != "hi" {
		t.Errorf("got %q", got)
	}
}

func TestClipBytesNeverSplitsRune(t *testing.T) {
	s := "aé☕😀x"
	for max := 0; max <= len(s)+1; max++ {
		got := ClipBytes(s, max)
		if !utf8.ValidString(got) {
			t.Errorf("ClipBytes(%q, %d) = %q is invalid UTF-8", s, max, got)
		}
		if len(got) > max && max >= 0 {
			t.Errorf("ClipBytes(%q, %d) = %q exceeds byte budget", s, max, got)
		}
	}
	if got := ClipBytes(s, len(s)); got != s {
		t.Errorf("full-budget clip changed string: %q", got)
	}
}
