// Package learn is the reconciler: it turns source documents and deterministic
// evidence into adjudicated claims. A claim becomes "current" only when
// corroborated by hard evidence or a fresh source; it is "conflicted" when it
// contradicts deterministic facts WITHIN ITS SCOPE, and "stale" when its source
// lagged the code. Nothing is promoted to truth on sight.
//
// All evidence is gathered from git-tracked / non-ignored files (see repofiles),
// so vendored, generated and gitignored files never count.
package learn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/repofiles"
	"github.com/memcode-ai/memcode/internal/sources"
	"github.com/memcode-ai/memcode/internal/store"
)

// Facts are hard, deterministic signals used to adjudicate softer claims.
// Scope-sensitive signals (e.g. TypeScript usage) are evaluated within a claim's
// scope at adjudication time, not stored as repo-wide booleans.
type Facts struct {
	PackageManager string // pnpm | yarn | npm | bun | ""
	Go             bool   // a go.mod exists
	CGODisabled    bool   // .goreleaser pins CGO_ENABLED=0
}

// Summary reports what a learn run produced.
type Summary struct {
	Total      int
	Current    int
	Conflicted int
	Stale      int
	Candidate  int
}

// Run discovers sources, extracts candidate claims (model), folds in
// deterministic claims, adjudicates against git-tracked evidence, and replaces
// the stored claim set.
func Run(ctx context.Context, st store.Store, runner *llm.Runner, root string) (Summary, error) {
	srcs, err := sources.Load(ctx, st)
	if err != nil {
		return Summary{}, err
	}
	staleByPath := map[string]bool{}
	for _, s := range srcs {
		staleByPath[s.Path] = s.Stale
	}

	files := repofiles.List(ctx, root) // honors .gitignore
	facts := detectFacts(files, root)
	claims := deterministicClaims(facts)

	candidates, err := extractClaims(ctx, runner, root, srcs)
	if err != nil {
		return Summary{}, err
	}
	claims = append(claims, adjudicate(files, facts, candidates, staleByPath)...)

	// Rebuild the claim set atomically: ClearClaims + the AddClaim batch run inside a
	// single transaction so a mid-loop failure (context cancel, a constraint error)
	// rolls back to the OLD claim set instead of leaving a partial new set. A
	// concurrent reader during the rebuild sees the old set until commit, never an
	// empty/partial one.
	var sum Summary
	err = st.RunInTx(ctx, func(tx store.Tx) error {
		if err := tx.ClearClaims(ctx); err != nil {
			return err
		}
		for _, c := range claims {
			if c.ID == "" {
				c.ID = "claim_" + randHex(4)
			}
			if err := tx.AddClaim(ctx, c); err != nil {
				return err
			}
			sum.Total++
			switch c.Status {
			case "current":
				sum.Current++
			case "conflicted":
				sum.Conflicted++
			case "stale":
				sum.Stale++
			case "candidate":
				sum.Candidate++
			}
		}
		return nil
	})
	return sum, err
}

func adjudicate(files []string, facts Facts, candidates []store.Claim, staleByPath map[string]bool) []store.Claim {
	out := make([]store.Claim, 0, len(candidates))
	for _, c := range candidates {
		c.Status, c.Confidence, c.Evidence = verdict(files, facts, c, staleByPath[c.SourcePath])
		out = append(out, c)
	}
	return out
}

func verdict(files []string, facts Facts, c store.Claim, sourceStale bool) (status, confidence, evidence string) {
	t := strings.ToLower(c.Text)

	// Package-manager dimension (repo-level).
	if pm := mentionedPM(t); pm != "" && facts.PackageManager != "" {
		if pm == facts.PackageManager {
			return "current", "high", "corroborated by lockfile (" + facts.PackageManager + ")"
		}
		return "conflicted", "high", "lockfile indicates " + facts.PackageManager + ", not " + pm
	}

	// TypeScript dimension — evaluated WITHIN the claim's scope.
	if strings.Contains(t, "typescript") {
		scopeName := c.Scope
		if scopeName == "" || scopeName == "." {
			scopeName = "the repo"
		}
		tsInScope := hasTypeScript(files, c.Scope)
		// A no-TS claim is an explicit NEGATION. Do NOT treat a bare "javascript" mention
		// as no-TS: the block is already gated on the claim containing "typescript", so a
		// claim naming both is almost always PRO-TS ("Prefer TypeScript over JavaScript") —
		// flagging it no-TS demoted a correct doctrine claim as conflicted every learn run.
		noTS := strings.Contains(t, "no typescript") || strings.Contains(t, "not typescript") ||
			strings.Contains(t, "avoid typescript") || strings.Contains(t, "without typescript") ||
			strings.Contains(t, "don't use typescript") || strings.Contains(t, "plain js") ||
			strings.Contains(t, "plain javascript") || strings.Contains(t, "vanilla javascript")
		switch {
		case noTS && !tsInScope:
			return "current", "high", "corroborated: no tracked .ts/tsconfig in " + scopeName
		case noTS && tsInScope:
			return "conflicted", "high", "tracked TypeScript present in " + scopeName
		case !noTS && tsInScope:
			return "current", "high", "corroborated: TypeScript present in " + scopeName
		case !noTS && !tsInScope:
			return "conflicted", "medium", "no tracked TypeScript in " + scopeName
		}
	}

	// No deterministic dimension: trust fresh sources, demote stale ones.
	if sourceStale {
		return "stale", "low", "source is stale (code changed after it)"
	}
	return "current", "medium", "from a current source (no contradicting evidence)"
}

// --- deterministic facts (over git-tracked, non-ignored files) ---

func detectFacts(files []string, root string) Facts {
	f := Facts{}
	for _, p := range files {
		switch filepath.Base(p) {
		case "pnpm-lock.yaml":
			if f.PackageManager == "" {
				f.PackageManager = "pnpm"
			}
		case "yarn.lock":
			if f.PackageManager == "" {
				f.PackageManager = "yarn"
			}
		case "bun.lockb":
			if f.PackageManager == "" {
				f.PackageManager = "bun"
			}
		case "package-lock.json":
			if f.PackageManager == "" {
				f.PackageManager = "npm"
			}
		case "go.mod":
			f.Go = true
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml")); err == nil && strings.Contains(string(b), "CGO_ENABLED=0") {
		f.CGODisabled = true
	}
	return f
}

func deterministicClaims(f Facts) []store.Claim {
	var cs []store.Claim
	add := func(typ, text, ev string) {
		cs = append(cs, store.Claim{Type: typ, Text: text, Scope: ".", Status: "current",
			Confidence: "high", Evidence: ev, SourcePath: "(detected)"})
	}
	if f.PackageManager != "" {
		add("command", "Use "+f.PackageManager+" for Node package management", f.PackageManager+" lockfile present")
	}
	if f.Go {
		add("command", "Build with `go build ./...` and test with `go test ./...`", "go.mod present")
	}
	if f.CGODisabled {
		add("doctrine", "Keep CGO_ENABLED=0 for static cross-platform binaries", ".goreleaser pins CGO_ENABLED=0")
	}
	return cs
}

// hasTypeScript reports whether any tracked TypeScript file lives within scope.
func hasTypeScript(files []string, scope string) bool {
	for _, p := range files {
		if !scopeGoverns(scope, p) {
			continue
		}
		base := filepath.Base(p)
		if base == "tsconfig.json" ||
			(strings.HasSuffix(p, ".ts") && !strings.HasSuffix(p, ".d.ts")) ||
			strings.HasSuffix(p, ".tsx") {
			return true
		}
	}
	return false
}

func scopeGoverns(scope, path string) bool {
	if scope == "" || scope == "." {
		return true
	}
	return path == scope || strings.HasPrefix(path, scope+"/")
}

func mentionedPM(t string) string {
	switch {
	case strings.Contains(t, "pnpm"):
		return "pnpm"
	case strings.Contains(t, "yarn"):
		return "yarn"
	case strings.Contains(t, "bun "):
		return "bun"
	case strings.Contains(t, "npm"):
		return "npm"
	}
	return ""
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
