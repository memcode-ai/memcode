package prefs

import (
	"path/filepath"
	"sort"

	"github.com/memcode-ai/memcode/internal/store"
)

// PendingPromotions returns candidates that have crossed the evidence bar and
// are ready to be promoted to standing plaintext rules: weight ≥
// promotionThreshold, ≥ minSignals signals, ≥ minSessions sessions, and status
// is still "candidate" (not already confirmed or demoted). Sorted by weight
// descending so the strongest preferences promote first.
func PendingPromotions(cands []Candidate) []Candidate {
	var out []Candidate
	for _, c := range cands {
		if c.Status != "" && c.Status != "candidate" {
			continue
		}
		// The "gating" axis is permission-adjacent. Doctrine: a preference never
		// tightens or loosens the permission gate, so a gating directive is captured
		// and visible via lookup but is NOT auto-promoted into a standing rule that
		// would shape permission behavior without the user's explicit say.
		if c.Axis == "gating" {
			continue
		}
		if c.Weight >= promotionThreshold && c.SignalCount >= minSignals && c.SessionCount >= minSessions {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Weight > out[j].Weight })
	return out
}

// PendingDemotions returns CONFIRMED candidates whose contradiction signal count
// (from the event log, via the reducer's Contradictions field) has crossed
// contradictionThreshold — the user has pushed back enough that the standing
// preference should be demoted. The caller passes the candidates from Reduce
// (which carries Contradictions); the store parameter is accepted for the v2
// case where demotion needs to re-scan the event log for fresh contradictions
// since the last Reduce. For v1 the candidates already carry the count.
func PendingDemotions(cands []Candidate, _ store.Store) ([]Candidate, error) {
	var out []Candidate
	for _, c := range cands {
		if c.Status == "confirmed" && c.Contradictions >= contradictionThreshold {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Contradictions > out[j].Contradictions })
	return out, nil
}

// ConfirmPath resolves the plaintext file for a confirmed preference under
// .memcode/prefs/. The file is written as <id>-<slug>.md (see writeConfirmedPref),
// so this globs <id>-*.md rather than guessing the slug — the previous <id>.md
// form never matched, so demotion deleted a nonexistent path and the binding file
// survived. Falls back to the legacy <id>.md if no slugged file is found.
func ConfirmPath(root, id string) string {
	dir := filepath.Join(root, ".memcode", "prefs")
	if matches, _ := filepath.Glob(filepath.Join(dir, id+"-*.md")); len(matches) > 0 {
		return matches[0]
	}
	return filepath.Join(dir, id+".md")
}
