package explore

import (
	"strings"
	"testing"
)

// TestReadersResolvesTheConcurrencyCap: 0 means "nothing configured", which must
// land on the default rather than on zero readers — a caller with no policy layer
// wired up would otherwise silently fan out to nobody.
func TestReadersResolvesTheConcurrencyCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{"unconfigured", 0, defaultReaders},
		{"negative is treated as unset", -3, defaultReaders},
		{"explicit single reader", 1, 1},
		{"explicit override", 12, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readers(tc.in); got != tc.want {
				t.Errorf("readers(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestIndentPreservesStructure: findings are indented into the synthesis prompt,
// so a blank answer must not become a line of stray spaces and a multi-line
// answer must keep every line aligned.
func TestIndentPreservesStructure(t *testing.T) {
	if got := indent(""); got != "" {
		t.Errorf("empty stays empty, got %q", got)
	}
	if got := indent("one"); got != "    one" {
		t.Errorf("single line = %q", got)
	}

	got := indent("first\nsecond\n\nfourth")
	want := "    first\n    second\n    \n    fourth"
	if got != want {
		t.Errorf("multi-line indent =\n%q\nwant\n%q", got, want)
	}
	for i, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "    ") {
			t.Errorf("line %d lost its indent: %q", i, line)
		}
	}
}
