// Package textutil holds small string helpers shared across packages.
//
// The truncation helpers exist because ad-hoc byte slicing (s[:n]) can split
// a multibyte UTF-8 rune, producing invalid UTF-8 in transcripts, tool
// results, and terminal output. Use these instead of slicing by byte.
package textutil

import "unicode/utf8"

// ClipRunes returns s truncated to at most max runes.
func ClipRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

// ClipRunesEllipsis returns s truncated to at most max runes, appending an
// ellipsis when truncation occurred.
func ClipRunesEllipsis(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return ClipRunes(s, max) + "…"
}

// ClipBytes returns s truncated to at most max bytes without splitting a
// UTF-8 rune: the cut backs up to the nearest rune boundary. Use when the
// budget is genuinely in bytes (wire limits, log caps).
func ClipBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
