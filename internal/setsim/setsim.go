// Package setsim holds set-similarity metrics shared by the layers that cluster
// text: preference clustering, mood's repetition detector, and lesson dedup.
//
// It exists because the same function had been written three times, in two map
// shapes, and had already started to diverge — one copy had dropped a guard the
// others kept. A similarity metric that decides whether two things are "the
// same" is exactly the kind of code that should have one definition and one set
// of tests.
package setsim

// Jaccard is the intersection-over-union of two sets, in [0,1].
//
// The value type is free so callers can keep whichever map shape they already
// use — map[string]bool and map[string]struct{} are both idiomatic set spellings
// and neither should have to convert to share this.
//
// Two empty sets score 0, not 1: callers use this to ask "is this new thing a
// repeat of that one", and an empty token set carries no evidence either way.
// Scoring it as a perfect match would make every content-free input look like a
// duplicate of every other.
func Jaccard[V any](a, b map[string]V) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	// len(a) and len(b) are both > 0 here, so union >= 1 and the division is safe.
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}
