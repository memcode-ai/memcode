package runtime

import (
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/sources"
	"github.com/memcode-ai/memcode/internal/structure"
)

// Orientation is the "I know where I am and what I know about this repo" summary
// the TUI shows on the opening screen, so the first moment proves memcode is
// repo-aware rather than a blank chatbot.
type Orientation struct {
	Repo             string
	Branch           string
	Subsystems       int
	Highlights       []string // top subsystem keys by activity (for a concrete prompt)
	ClaimsCurrent    int
	ClaimsStale      int
	ClaimsConflicted int
	Sources          int
}

// Orientation gathers the deterministic repo summary (no model call).
func (s *Session) Orientation(ctx context.Context) Orientation {
	o := Orientation{Repo: filepath.Base(s.root), Branch: branchName(ctx, s.root)}

	if topo, err := structure.Load(ctx, s.store); err == nil {
		o.Subsystems = len(topo.Subsystems)
		subs := append([]structure.Subsystem(nil), topo.Subsystems...)
		sort.Slice(subs, func(i, j int) bool { return subs[i].Commits > subs[j].Commits })
		for i, sub := range subs {
			if i >= 3 {
				break
			}
			o.Highlights = append(o.Highlights, sub.Key)
		}
	}
	if claims, err := s.store.ListClaims(ctx); err == nil {
		for _, c := range claims {
			switch c.Status {
			case "current":
				o.ClaimsCurrent++
			case "stale":
				o.ClaimsStale++
			case "conflicted":
				o.ClaimsConflicted++
			}
		}
	}
	if srcs, err := sources.Load(ctx, s.store); err == nil {
		o.Sources = len(srcs)
	}
	return o
}

func branchName(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
