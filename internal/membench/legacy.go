//go:build membench

package membench

import (
	"strings"

	"github.com/memcode-ai/memcode/internal/sessionlog"
)

// LegacyAdapter reimplements the pre-2026-07-25 sessionlog.Search verbatim
// (case-insensitive contiguous substring, newest first, no ranking) so the
// audit's "before" number stays reproducible after the product moved on.
//
// Bench-only history, gated behind `-tags membench` so it never compiles into
// a release binary; without the tag the stub in legacy_stub.go stands in.
type LegacyAdapter struct{}

func (LegacyAdapter) Name() string { return "legacy" }

func (LegacyAdapter) Rank(root string, q Question, docs []SessionDoc, k int) ([]string, error) {
	needle := strings.ToLower(strings.TrimSpace(q.Text))
	var out []string
	// Newest session first, mirroring the old scan's file ordering.
	for i := len(docs) - 1; i >= 0; i-- {
		recs, err := sessionlog.Recent(root, docs[i].ID, 0)
		if err != nil {
			continue
		}
		for j := len(recs) - 1; j >= 0; j-- {
			r := recs[j]
			hay := strings.ToLower(r.Text + "\x00" + r.Content + "\x00" + r.Input + "\x00" + r.Tool)
			if strings.Contains(hay, needle) && r.Slug != "" {
				out = append(out, r.Slug)
				if k > 0 && len(out) >= k {
					return out, nil
				}
			}
		}
	}
	return out, nil
}
