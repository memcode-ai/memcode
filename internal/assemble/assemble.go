// Package assemble is the context compiler. It does NOT retrieve memories — it
// *compiles* a structured ContextPack the way a human orients before touching
// code: which subsystem, what it depends on, the goals in play, recent activity,
// the files worth reading. The pack is a typed object (with provenance on every
// item) so the future agent runtime consumes the same thing the CLI renders.
//
// v1 is fully deterministic: topology + objectives + git recency + a Go-native
// content search. No model calls, no embeddings.
package assemble

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/memcode-ai/memcode/internal/gitlog"
	"github.com/memcode-ai/memcode/internal/objectives"
	"github.com/memcode-ai/memcode/internal/sources"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
)

// Default budget for the assembled pack (rough heuristic; ~4 chars/token).
const defaultTokenBudget = 8000

// Item is one included piece of context, always carrying the reason it's here.
type Item struct {
	Ref    string `json:"ref"`             // path / subsystem key / objective id / commit hash
	Label  string `json:"label,omitempty"` // human text (title, commit subject, …)
	Reason string `json:"reason"`          // provenance: why this was included
	Score  int    `json:"score,omitempty"` // ranking score (higher = more relevant)
}

// Budget reports the rough token cost of the recommended reads.
type Budget struct {
	Limit     int  `json:"limit"`
	Estimated int  `json:"estimated"`
	Truncated bool `json:"truncated"`
}

// ContextPack is the structured output of the compiler — consumed by both the
// CLI renderer and (later) the agent's orient step.
type ContextPack struct {
	Target           string   `json:"target"`
	Subsystem        string   `json:"subsystem,omitempty"`
	Ecosystem        string   `json:"ecosystem,omitempty"`
	Objectives       []Item   `json:"objectives"`
	Dependencies     []Item   `json:"dependencies"`
	RelevantFiles    []Item   `json:"relevant_files"`
	RecentEvents     []Item   `json:"recent_events"`
	Constraints      []Item   `json:"constraints"`
	RecommendedReads []Item   `json:"recommended_reads"`
	RankingReasons   []string `json:"ranking_reasons"`
	TokenBudget      Budget   `json:"token_budget"`
}

// Context compiles a ContextPack for a target, which may be a path (file or
// directory) or a free-text query.
func Context(ctx context.Context, st store.Store, root, target string) (ContextPack, error) {
	topo, err := structure.Load(ctx, st)
	if err != nil {
		return ContextPack{}, err
	}

	pack := ContextPack{
		Target:      target,
		TokenBudget: Budget{Limit: defaultTokenBudget},
	}

	relPath, isPath := structure.ResolvePath(root, target)
	var sub structure.Subsystem
	var haveSub bool

	if isPath {
		sub, haveSub = structure.ContainingSubsystem(topo.Subsystems, relPath)
		pack.RankingReasons = append(pack.RankingReasons,
			"target is a path; subsystem resolved by directory containment")
	}

	// Relevant files.
	if isPath {
		pack.RelevantFiles = append(pack.RelevantFiles, targetFiles(root, relPath)...)
	} else {
		hits := searchFiles(ctx, root, target)
		pack.RankingReasons = append(pack.RankingReasons,
			"target is a query; files ranked by content matches")
		for _, h := range hits {
			pack.RelevantFiles = append(pack.RelevantFiles, Item{
				Ref:    h.path,
				Reason: pluralMatches(h.count, target),
				Score:  100 + h.count,
			})
		}
		if len(hits) > 0 {
			sub, haveSub = structure.ContainingSubsystem(topo.Subsystems, hits[0].path)
		}
	}

	if haveSub {
		pack.Subsystem = sub.Key
		pack.Ecosystem = sub.Ecosystem
		// Subsystem key files are always worth surfacing.
		pack.RelevantFiles = append(pack.RelevantFiles, keyFiles(root, sub)...)
		pack.Dependencies = dependencyItems(topo, sub.Key)
	}

	pack.RelevantFiles = dedupRankCap(pack.RelevantFiles, 12)

	// Objectives in flight (repo-level in v1; not yet linked to subsystems).
	if cur, err := objectives.New(st).Current(ctx); err == nil {
		for _, o := range cur {
			pack.Objectives = append(pack.Objectives, Item{
				Ref:    o.ID,
				Label:  o.Title,
				Reason: "active objective (" + o.Status + ")",
			})
		}
	}

	// Recent activity from git history for the target path / subsystem.
	historyPath := relPath
	if !isPath && haveSub {
		historyPath = sub.Key
	}
	for _, c := range gitlog.Recent(ctx, root, historyPath, 5) {
		pack.RecentEvents = append(pack.RecentEvents, Item{
			Ref:    c.Hash,
			Label:  c.Subject,
			Reason: "recent change by " + c.Author + " (" + c.Date + ")",
		})
	}

	// Constraints come from discovered instruction/doc sources (doctrine
	// candidates) that GOVERN this target's scope. Stale ones are flagged, not
	// hidden — the agent should know an instruction may be out of date.
	constraintTarget := relPath
	if constraintTarget == "" {
		constraintTarget = sub.Key
	}
	// Prefer adjudicated claims (from `learn`): current ones are constraints,
	// conflicted/stale ones are warnings. Fall back to raw sources otherwise.
	if claims, err := st.ListClaims(ctx); err == nil && len(claims) > 0 {
		for _, c := range claims {
			if !scopeGoverns(c.Scope, constraintTarget) {
				continue
			}
			switch c.Status {
			case "current":
				pack.Constraints = append(pack.Constraints, Item{
					Ref: c.Text, Reason: c.Type + " · " + c.Confidence + " · " + baseName(c.SourcePath)})
			case "conflicted", "stale":
				pack.Constraints = append(pack.Constraints, Item{
					Ref: c.Text, Reason: "⚠ " + c.Status + " — " + c.Evidence})
			}
		}
	} else if srcs, err := sources.Load(ctx, st); err == nil {
		for _, s := range srcs {
			if !s.AppliesTo(constraintTarget) {
				continue
			}
			reason := s.Kind + " instructions (" + s.Scope + ")"
			if s.Stale {
				reason += " — ⚠ possibly STALE"
			}
			pack.Constraints = append(pack.Constraints, Item{Ref: s.Path, Reason: reason})
		}
	}
	if len(pack.Constraints) == 0 {
		pack.RankingReasons = append(pack.RankingReasons,
			"no claims/sources govern this area (run `memcode learn`)")
	}

	// Recommended reads (ordered): project intent, subsystem manifest, the target.
	pack.RecommendedReads = recommendedReads(root, topo, sub, haveSub, relPath, isPath)
	pack.TokenBudget = budgetFor(root, pack.RecommendedReads)

	pack.RankingReasons = append(pack.RankingReasons,
		"ranking: target/match > subsystem key files; reads ordered intent→manifest→target")
	pack.normalize()
	return pack, nil
}

// normalize ensures list fields marshal as [] rather than null, so consumers
// (the agent, JSON clients) get a stable shape.
func (p *ContextPack) normalize() {
	if p.Objectives == nil {
		p.Objectives = []Item{}
	}
	if p.Dependencies == nil {
		p.Dependencies = []Item{}
	}
	if p.RelevantFiles == nil {
		p.RelevantFiles = []Item{}
	}
	if p.RecentEvents == nil {
		p.RecentEvents = []Item{}
	}
	if p.Constraints == nil {
		p.Constraints = []Item{}
	}
	if p.RecommendedReads == nil {
		p.RecommendedReads = []Item{}
	}
	if p.RankingReasons == nil {
		p.RankingReasons = []string{}
	}
}

// scopeGoverns reports whether a claim/source scope governs the target path.
func scopeGoverns(scope, target string) bool {
	if scope == "" || scope == "." {
		return true
	}
	return target == scope || strings.HasPrefix(target, scope+"/")
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func dependencyItems(topo structure.Result, key string) []Item {
	var out []Item
	for _, d := range topo.Deps {
		switch {
		case d.From == key:
			out = append(out, Item{Ref: d.To, Reason: "this subsystem depends on it"})
		case d.To == key:
			out = append(out, Item{Ref: d.From, Reason: "depends on this subsystem"})
		}
	}
	return out
}

// dedupRankCap removes duplicate paths (keeping the highest score), sorts by
// score desc, and caps the slice.
func dedupRankCap(items []Item, cap int) []Item {
	best := map[string]Item{}
	for _, it := range items {
		if cur, ok := best[it.Ref]; !ok || it.Score > cur.Score {
			best[it.Ref] = it
		}
	}
	out := make([]Item, 0, len(best))
	for _, it := range best {
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Ref < out[j].Ref
	})
	if len(out) > cap {
		out = out[:cap]
	}
	return out
}

func pluralMatches(n int, q string) string {
	if n == 1 {
		return "1 match for \"" + q + "\""
	}
	return strconv.Itoa(n) + " matches for \"" + q + "\""
}
