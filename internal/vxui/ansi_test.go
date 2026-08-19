package vxui

// Tests for the live ANSI/SGR machinery (ansi.go): parseANSISpans drives the
// theme picker's live sample rendering, applySGR its attribute/color state
// machine, color256 the xterm-256 → RGB mapping. These were untested while a
// dead normalizer (sgr.go, since deleted) carried the only SGR tests.

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"
)

func TestParseANSISpansPlainText(t *testing.T) {
	spans := parseANSISpans("plain text, no escapes")
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d: %+v", len(spans), spans)
	}
	if spans[0].Text != "plain text, no escapes" {
		t.Fatalf("text = %q", spans[0].Text)
	}
	if spans[0].Style != (ui.Style{}) {
		t.Fatalf("plain text must carry the zero style, got %+v", spans[0].Style)
	}
}

func TestParseANSISpansBasicSGRRuns(t *testing.T) {
	// bold red "ERR", reset, then plain " ok".
	spans := parseANSISpans("\x1b[1;31mERR\x1b[0m ok")
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d: %+v", len(spans), spans)
	}
	if spans[0].Text != "ERR" {
		t.Fatalf("span 0 text = %q", spans[0].Text)
	}
	if spans[0].Style.Attribute&ui.AttrBold == 0 {
		t.Fatalf("span 0 must be bold, got %+v", spans[0].Style)
	}
	if want := color256(1); spans[0].Style.Foreground != want {
		t.Fatalf("span 0 foreground = %v, want %v (red)", spans[0].Style.Foreground, want)
	}
	if spans[1].Text != " ok" {
		t.Fatalf("span 1 text = %q", spans[1].Text)
	}
	if spans[1].Style != (ui.Style{}) {
		t.Fatalf("reset must clear the style, got %+v", spans[1].Style)
	}
}

func TestParseANSISpans256Color(t *testing.T) {
	spans := parseANSISpans("\x1b[38;5;196mX")
	if len(spans) != 1 || spans[0].Text != "X" {
		t.Fatalf("spans = %+v", spans)
	}
	// xterm 196 is pure red in the 6×6×6 cube.
	if want := ui.RGB(255, 0, 0); spans[0].Style.Foreground != want {
		t.Fatalf("foreground = %v, want %v", spans[0].Style.Foreground, want)
	}
}

func TestParseANSISpansTruecolorAndBackground(t *testing.T) {
	spans := parseANSISpans("\x1b[38;2;10;20;30m\x1b[48;5;16mX")
	if len(spans) != 1 {
		t.Fatalf("spans = %+v", spans)
	}
	if want := ui.RGB(10, 20, 30); spans[0].Style.Foreground != want {
		t.Fatalf("foreground = %v, want %v", spans[0].Style.Foreground, want)
	}
	if want := color256(16); spans[0].Style.Background != want {
		t.Fatalf("background = %v, want %v", spans[0].Style.Background, want)
	}
}

func TestApplySGR(t *testing.T) {
	var st ui.Style

	applySGR(&st, "1") // bold
	if st.Attribute&ui.AttrBold == 0 {
		t.Fatalf("1 must set bold: %+v", st)
	}
	applySGR(&st, "22") // bold off
	if st.Attribute&ui.AttrBold != 0 {
		t.Fatalf("22 must clear bold: %+v", st)
	}

	applySGR(&st, "94") // bright blue fg
	if want := color256(12); st.Foreground != want {
		t.Fatalf("94 foreground = %v, want %v", st.Foreground, want)
	}
	applySGR(&st, "39") // default fg
	if st.Foreground != 0 {
		t.Fatalf("39 must reset the foreground: %+v", st)
	}

	applySGR(&st, "42") // green bg
	if want := color256(2); st.Background != want {
		t.Fatalf("42 background = %v, want %v", st.Background, want)
	}
	applySGR(&st, "49") // default bg
	if st.Background != 0 {
		t.Fatalf("49 must reset the background: %+v", st)
	}

	// Empty params = reset (ESC[m is shorthand for ESC[0m).
	applySGR(&st, "1;31")
	applySGR(&st, "")
	if st != (ui.Style{}) {
		t.Fatalf("empty params must reset: %+v", st)
	}

	// A combined run: bold + truecolor fg + 256 bg in one body.
	applySGR(&st, "1;38;2;1;2;3;48;5;196")
	if st.Attribute&ui.AttrBold == 0 {
		t.Fatalf("combined run lost bold: %+v", st)
	}
	if want := ui.RGB(1, 2, 3); st.Foreground != want {
		t.Fatalf("combined run foreground = %v, want %v", st.Foreground, want)
	}
	if want := ui.RGB(255, 0, 0); st.Background != want {
		t.Fatalf("combined run background = %v, want %v", st.Background, want)
	}
}

func TestColor256(t *testing.T) {
	cases := []struct {
		n    int
		want ui.Color
	}{
		{0, ui.RGB(0, 0, 0)},         // basic black
		{1, ui.RGB(0x80, 0, 0)},      // basic red
		{15, ui.RGB(255, 255, 255)},  // bright white
		{16, ui.RGB(0, 0, 0)},        // cube origin
		{196, ui.RGB(255, 0, 0)},     // cube pure red
		{231, ui.RGB(255, 255, 255)}, // cube white
		{232, ui.RGB(8, 8, 8)},       // grayscale start
		{255, ui.RGB(238, 238, 238)}, // grayscale end
	}
	for _, c := range cases {
		if got := color256(c.n); got != c.want {
			t.Errorf("color256(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}
