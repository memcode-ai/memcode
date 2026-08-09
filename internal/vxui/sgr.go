// Package vxui holds the vaxis-based renderer spike for memcode's TUI migration.
// It exists to prove, with real compiling/passing code, that vaxis PrimaryScreen gives
// memcode the model it needs (a persistent themed live region + durable native scrollback,
// no cursor probes) AND that memcode's existing lipgloss-styled output renders correctly.
package vxui

import "strings"

// NormalizeSGR rewrites lipgloss-style SGR sequences into the form vaxis's styled-string
// parser understands. lipgloss emits extended colors in the legacy SEMICOLON encoding —
// ESC[38;2;R;G;Bm (truecolor) and ESC[38;5;Nm (256) — but vaxis only reads the ITU
// COLON-subparameter form (ESC[38:2:R:G:Bm) for 38/48/58. Without this conversion vaxis
// would mis-parse a lipgloss truecolor run (the "2" becomes faint, the channels become
// garbage, the foreground never sets), silently dropping every theme color.
//
// It only joins the 38/48/58 color runs; attributes and the basic 30-37/90-97 colors are
// single params and pass through untouched. Text outside ESC[…m sequences is copied verbatim.
func NormalizeSGR(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		rest := s[i+2:]
		j := strings.IndexByte(rest, 'm')
		if j < 0 { // unterminated CSI — emit the remainder verbatim
			b.WriteString(s[i:])
			return b.String()
		}
		b.WriteString("\x1b[")
		b.WriteString(joinExtendedColors(rest[:j]))
		b.WriteByte('m')
		s = rest[j+1:]
	}
}

// joinExtendedColors collapses "38;5;N" and "38;2;R;G;B" (and 48/58) runs in a single SGR
// body into colon form, leaving all other params semicolon-separated as vaxis expects.
func joinExtendedColors(body string) string {
	params := strings.Split(body, ";")
	out := make([]string, 0, len(params))
	for i := 0; i < len(params); i++ {
		p := params[i]
		if (p == "38" || p == "48" || p == "58") && i+1 < len(params) {
			switch params[i+1] {
			case "5":
				if i+2 < len(params) {
					out = append(out, p+":5:"+params[i+2])
					i += 2
					continue
				}
			case "2":
				if i+4 < len(params) {
					out = append(out, p+":2:"+params[i+2]+":"+params[i+3]+":"+params[i+4])
					i += 4
					continue
				}
			}
		}
		out = append(out, p)
	}
	return strings.Join(out, ";")
}
