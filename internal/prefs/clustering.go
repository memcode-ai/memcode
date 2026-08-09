package prefs

import (
	"sort"
	"strings"
)

// cluster groups signals by axis, then within each axis groups by lexical
// similarity (Jaccard token overlap ≥ jaccardThreshold). Signals with opposite
// polarity ("always commit" vs "never commit") are kept in SEPARATE clusters —
// they're contradictions, not merges — but both clusters record the other's
// polarity as a contradiction count via buildCandidate.
//
// v1 approach: lexical Jaccard. The repo's doctrine is "lexical until measured
// otherwise"; BM25 via cli/internal/recall/ is the v2 escalation if false
// negatives (paraphrases sharing no tokens) prove costly. The `axis` field helps
// — two signals on the same axis are already candidates to merge.
func clusterSignals(signals []signalEvent) []cluster {
	// First partition by axis + polarity so contradictions stay separate.
	byKey := map[string][]signalEvent{}
	for _, sig := range signals {
		key := sig.Axis + "\x00" + string(rune(polarity(sig.Text)+1))
		byKey[key] = append(byKey[key], sig)
	}

	// Within each (axis, polarity) partition, merge by Jaccard overlap.
	var out []cluster
	for key, group := range byKey {
		axis := key[:strings.IndexByte(key, '\x00')]
		clusters := jaccardGroup(group)
		for _, cl := range clusters {
			out = append(out, cluster{
				axis:     axis,
				polarity: polarity(cl[0].Text),
				scope:    dominantScope(cl),
				signals:  cl,
			})
		}
	}
	// Sort for deterministic output (tests rely on stable order within an axis).
	sort.Slice(out, func(i, j int) bool {
		if out[i].axis != out[j].axis {
			return out[i].axis < out[j].axis
		}
		return len(out[i].signals) > len(out[j].signals)
	})
	return out
}

// cluster is the reducer's intermediate: a group of signals on the same axis with
// the same polarity that lexically overlap enough to be the same preference.
type cluster struct {
	axis     string
	polarity int // +1 affirmative, -1 negated
	scope    string
	signals  []signalEvent
}

// jaccardGroup merges signals into clusters by greedy Jaccard overlap. Each
// signal starts its own cluster; it merges into the first existing cluster whose
// representative shares ≥ jaccardThreshold token overlap. O(n²) worst case, but n
// is bounded by maxScanSignals and partitioned by axis+polarity first.
func jaccardGroup(signals []signalEvent) [][]signalEvent {
	var clusters [][]signalEvent
	clusterTokens := make([]map[string]bool, 0)
	for _, sig := range signals {
		toks := tokens(sig.Text)
		merged := false
		for i, ct := range clusterTokens {
			if jaccard(toks, ct) >= jaccardThreshold {
				clusters[i] = append(clusters[i], sig)
				// Refresh the representative tokens so later signals can match the
				// growing cluster.
				for t := range toks {
					ct[t] = true
				}
				merged = true
				break
			}
		}
		if !merged {
			clusters = append(clusters, []signalEvent{sig})
			clusterTokens = append(clusterTokens, toks)
		}
	}
	return clusters
}

// tokens lowercases and splits on non-alphanumerics, returning a set.
func tokens(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(w) >= 2 { // skip single-char tokens (noise)
			set[w] = true
		}
	}
	return set
}

// jaccard is the token-set Jaccard similarity |A∩B| / |A∪B|.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// polarity returns +1 for affirmative directives ("always", "use", "prefer") and
// -1 for negated ones ("never", "stop", "don't", "no more", "avoid"). Same-axis
// signals with opposite polarity are contradictions, not merges.
func polarity(text string) int {
	t := strings.ToLower(text)
	negators := []string{"never", "stop", "don't", "dont", "no more", "avoid", "not "}
	for _, neg := range negators {
		if strings.Contains(t, neg) {
			return -1
		}
	}
	// "always" / "use" / "prefer" are explicit affirmatives; absence is still +1
	// (a bare "commit, deploy, rebuild" is affirmative).
	return 1
}

// dominantScope returns the most common scope among the cluster's signals,
// defaulting to "." for empty scopes.
func dominantScope(signals []signalEvent) string {
	counts := map[string]int{}
	for _, sig := range signals {
		s := sig.Scope
		if s == "" {
			s = "."
		}
		counts[s]++
	}
	best := "."
	bestN := 0
	for s, n := range counts {
		if n > bestN || (n == bestN && s == ".") {
			best = s
			bestN = n
		}
	}
	return best
}
