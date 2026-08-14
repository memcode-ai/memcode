package channels

// Chunk splits s into pieces of at most max runes, preferring to break at a
// newline near the limit so code blocks and paragraphs aren't cut mid-line. It
// is the ONE splitter every adapter shares: Hermes and OpenClaw both grew
// message-too-long bugs precisely where a side path bypassed the shared chunker
// (or a second, divergent splitter stripped indentation differently), so all
// outbound text goes through here.
//
// An empty string yields a single empty piece, and the split is loss-free: the
// concatenation of the result always equals the input (only the exact newline we
// break on moves to the end of a piece, never dropped).
func Chunk(s string, max int) []string {
	if max <= 0 {
		return []string{s}
	}
	var parts []string
	r := []rune(s)
	for len(r) > max {
		cut := max
		// Prefer the last newline in the window so we don't split mid-line, but
		// only if it's not so early that we'd waste most of the budget.
		if nl := lastIndexRune(r[:max], '\n'); nl > max/2 {
			cut = nl + 1
		}
		parts = append(parts, string(r[:cut]))
		r = r[cut:]
	}
	return append(parts, string(r))
}

func lastIndexRune(r []rune, target rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == target {
			return i
		}
	}
	return -1
}
