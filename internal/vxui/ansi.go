package vxui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"
)

// parseANSISpans converts an ANSI/SGR-styled string (e.g. RenderThemeSample output) into ui
// TextSpans, so styled terminal output can be drawn into the live-region widget tree.
func parseANSISpans(s string) []ui.TextSpan {
	var spans []ui.TextSpan
	var cur ui.Style
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			spans = append(spans, ui.TextSpan{Text: buf.String(), Style: cur})
			buf.Reset()
		}
	}
	rs := []rune(s)
	for i := 0; i < len(rs); {
		if rs[i] == '\x1b' && i+1 < len(rs) && rs[i+1] == '[' {
			j := i + 2
			for j < len(rs) && rs[j] != 'm' && rs[j] != '\x1b' {
				j++
			}
			if j < len(rs) && rs[j] == 'm' {
				flush()
				applySGR(&cur, string(rs[i+2:j]))
				i = j + 1
				continue
			}
		}
		buf.WriteRune(rs[i])
		i++
	}
	flush()
	return spans
}

func applySGR(st *ui.Style, params string) {
	if params == "" {
		params = "0"
	}
	f := strings.Split(params, ";")
	for k := 0; k < len(f); k++ {
		n, _ := strconv.Atoi(f[k])
		switch {
		case n == 0:
			*st = ui.Style{}
		case n == 1:
			st.Attribute |= ui.AttrBold
		case n == 22:
			st.Attribute &^= ui.AttrBold
		case n == 39:
			st.Foreground = 0
		case n == 49:
			st.Background = 0
		case n >= 30 && n <= 37:
			st.Foreground = color256(n - 30)
		case n >= 90 && n <= 97:
			st.Foreground = color256(n - 90 + 8)
		case n >= 40 && n <= 47:
			st.Background = color256(n - 40)
		case n >= 100 && n <= 107:
			st.Background = color256(n - 100 + 8)
		case (n == 38 || n == 48) && k+1 < len(f):
			fg := n == 38
			if f[k+1] == "2" && k+4 < len(f) {
				r, _ := strconv.Atoi(f[k+2])
				g, _ := strconv.Atoi(f[k+3])
				b, _ := strconv.Atoi(f[k+4])
				c := ui.RGB(uint8(r), uint8(g), uint8(b))
				if fg {
					st.Foreground = c
				} else {
					st.Background = c
				}
				k += 4
			} else if f[k+1] == "5" && k+2 < len(f) {
				idx, _ := strconv.Atoi(f[k+2])
				c := color256(idx)
				if fg {
					st.Foreground = c
				} else {
					st.Background = c
				}
				k += 2
			}
		}
	}
}

// color256 maps an xterm 256-color index to a ui.Color (RGB).
func color256(n int) ui.Color {
	switch {
	case n < 16:
		base := []uint32{0x000000, 0x800000, 0x008000, 0x808000, 0x000080, 0x800080, 0x008080, 0xC0C0C0,
			0x808080, 0xFF0000, 0x00FF00, 0xFFFF00, 0x0000FF, 0xFF00FF, 0x00FFFF, 0xFFFFFF}
		v := base[n]
		return ui.RGB(uint8(v>>16), uint8(v>>8), uint8(v))
	case n < 232:
		n -= 16
		sc := func(x int) uint8 {
			if x == 0 {
				return 0
			}
			return uint8(55 + x*40)
		}
		return ui.RGB(sc(n/36), sc((n%36)/6), sc(n%6))
	default:
		v := uint8(8 + (n-232)*10)
		return ui.RGB(v, v, v)
	}
}

// ansiSGR builds a truecolor SGR prefix from a "#RRGGBB" palette hex (bold optional).
func ansiSGR(hex string, bold bool) string {
	h := strings.TrimPrefix(hex, "#")
	if len(h) != 6 {
		return ""
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return ""
	}
	b := ""
	if bold {
		b = "1;"
	}
	return fmt.Sprintf("\x1b[%s38;2;%d;%d;%dm", b, (v>>16)&0xff, (v>>8)&0xff, v&0xff)
}
