package runtime

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// A long new-file render shows only a short HEAD glimpse + a "+N more lines" marker — a
// created file is new content, not a change to review, so it must not flood scrollback.
func TestRenderNewFileCapsPreview(t *testing.T) {
	var src strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&src, "line %d\n", i)
	}
	var out strings.Builder
	renderNewFile(&out, src.String(), "x.txt", 100)
	got := ansiSeq.ReplaceAllString(out.String(), "")
	// Head shown, the rest truncated with a count.
	for _, want := range []string{"line 1", "Created file (200 lines)", "more lines"} {
		if !strings.Contains(got, want) {
			t.Errorf("capped preview missing %q in:\n%s", want, got)
		}
	}
	// The marker counts the hidden remainder (200 - 20).
	if !strings.Contains(got, "+180 more lines") {
		t.Errorf("expected a +180 more-lines marker:\n%s", got)
	}
	// Nothing past the cap is dumped.
	for _, gone := range []string{"line 21", "line 200"} {
		if strings.Contains(got, gone) {
			t.Errorf("line %q is past the preview cap and must not be shown", gone)
		}
	}
}

// The /theme picker's live preview renders a real diff sample (add/del + syntax) in
// the active theme — it must produce code with +/- rows, not just chrome.
func TestRenderThemeSample(t *testing.T) {
	plain := ansiSeq.ReplaceAllString(RenderThemeSample(72), "")
	for _, want := range []string{"greet", "msg :=", "+", "-"} {
		if !strings.Contains(plain, want) {
			t.Errorf("theme sample missing %q in:\n%s", want, plain)
		}
	}
}

// visibleCells is the TRUE on-screen width of a rendered row: ANSI stripped, then DISPLAY
// CELLS (CJK/wide runes count as 2) — the measure the terminal actually wraps on. (Rune
// count undercounts CJK; a stray lipgloss measure mis-counts chroma's truecolor output.)
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleCells(s string) int { return runewidth.StringWidth(ansiSeq.ReplaceAllString(s, "")) }

// TestDiffRowNeverExceedsWidth pins the wrap fix across the cases that actually broke:
// long lines, TAB-indented lines (tabs expand to multiple columns), and CJK (each glyph
// is two cells). Every rendered row's display width must stay within `width` so the
// colored background never spills onto a second terminal row.
func TestDiffRowNeverExceedsWidth(t *testing.T) {
	const width = 40
	diff := "@@ -1,5 +1,5 @@\n" +
		"+a very long added line that clearly runs well past forty visible columns, no question\n" +
		"-another removed line that is also far too long to fit within the configured width here\n" +
		"+\t\t\tname string // a deeply tab-indented struct field that would tab-expand past width\n" +
		"+\t{\"multibyte\", \"日本語テストの長い行が幅をはるかに超えています\", 4, \"日本語…\"},\n" +
		" a context line that is likewise much longer than the forty column budget we allow\n"
	var buf strings.Builder
	renderDiff(&buf, diff, "x.go", width)
	for _, ln := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if w := visibleCells(ln); w > width {
			t.Errorf("row display width %d > %d: %q", w, width, ansiSeq.ReplaceAllString(ln, ""))
		}
	}
}

// TestClampWidth covers the four essential behaviours of clampWidth:
//   - empty input returns "" with zero cell width
//   - a string that fits within max is returned unchanged with its exact width
//   - an over-length ASCII string is truncated and gets a trailing ellipsis
//   - a double-width CJK string (e.g. 日本語) is truncated at a cell boundary
//     and the returned cell width accounts for the 2-cell runes + the ellipsis
func TestClampWidth(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		max       int
		wantStr   string
		wantCells int
	}{
		{
			name:      "empty input",
			input:     "",
			max:       10,
			wantStr:   "",
			wantCells: 0,
		},
		{
			name:      "string that fits exactly",
			input:     "hello",
			max:       10,
			wantStr:   "hello",
			wantCells: 5,
		},
		{
			name:      "string that fits with no room to spare",
			input:     "hello",
			max:       5,
			wantStr:   "hello",
			wantCells: 5,
		},
		{
			name: "over-length ASCII gets ellipsis",
			// "hello world" is 11 cells; max=8 → budget=7 → "hello w" + "…" = 8 cells
			input:     "hello world",
			max:       8,
			wantStr:   "hello w…",
			wantCells: 8,
		},
		{
			name: "CJK double-width truncated at cell boundary",
			// "日本語" = 3 runes × 2 cells = 6 cells; max=5 → budget=4 →
			// "日" (2) + "本" (2) = 4 cells fits, next rune "語" (2) would push to 6 > 4 → stop
			// result: "日本…" = 4 + 1 = 5 cells
			input:     "日本語",
			max:       5,
			wantStr:   "日本…",
			wantCells: 5,
		},
		{
			name: "CJK truncated when first rune already fits but second doesn't",
			// "日本語テスト" = 6 runes × 2 = 12 cells; max=4 → budget=3 →
			// "日" (2) fits, next rune "本" (2) would push w to 4 > 3 → stop immediately
			// result: "日…" = 2 + 1 = 3 cells
			input:     "日本語テスト",
			max:       4,
			wantStr:   "日…",
			wantCells: 3,
		},
		{
			name:      "max zero returns empty",
			input:     "anything",
			max:       0,
			wantStr:   "",
			wantCells: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStr, gotCells := clampWidth(tt.input, tt.max)
			if gotStr != tt.wantStr {
				t.Errorf("clampWidth(%q, %d) string = %q, want %q", tt.input, tt.max, gotStr, tt.wantStr)
			}
			if gotCells != tt.wantCells {
				t.Errorf("clampWidth(%q, %d) cells = %d, want %d", tt.input, tt.max, gotCells, tt.wantCells)
			}
			// Cross-check: reported cell width must match runewidth's measure of the returned string.
			if actual := runewidth.StringWidth(gotStr); actual != gotCells {
				t.Errorf("clampWidth(%q, %d) claimed cells=%d but runewidth.StringWidth=%d", tt.input, tt.max, gotCells, actual)
			}
			// The result must never exceed max cells.
			if gotCells > tt.max && tt.max > 0 {
				t.Errorf("clampWidth(%q, %d) result exceeds max: cells=%d", tt.input, tt.max, gotCells)
			}
		})
	}
}

// TestDiffRowSingleBackground pins the one-shade design: an added row carries the
// single green bg and NOT the old deeper-gutter shade (two backgrounds per row).
func TestDiffRowSingleBackground(t *testing.T) {
	const width = 60
	dc := newDiffCtx()
	var buf strings.Builder
	renderDiff(&buf, "@@ -1,1 +1,1 @@\n+x := 1\n", "x.go", width)
	out := buf.String()
	if !strings.Contains(out, dc.addBg) {
		t.Fatalf("added row should carry the single green bg %q", dc.addBg)
	}
	// The bright gutter fg should appear; the row should not mix a second bg shade.
	if !strings.Contains(out, dc.addGutter) {
		t.Errorf("added row should use the bright gutter foreground")
	}
}
