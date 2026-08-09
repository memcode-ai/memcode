package learn

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/store"
)

func TestVerdict(t *testing.T) {
	cases := []struct {
		name       string
		files      []string
		facts      Facts
		text       string
		scope      string
		stale      bool
		wantStatus string
	}{
		{"pnpm corroborated", nil, Facts{PackageManager: "pnpm"}, "Use pnpm for packages", ".", false, "current"},
		{"npm contradicts pnpm", nil, Facts{PackageManager: "pnpm"}, "Run npm install", ".", false, "conflicted"},

		// TypeScript is judged WITHIN the claim's scope.
		{"no-ts corroborated in scope", []string{"apps/web/page.jsx"}, Facts{}, "JavaScript only, no TypeScript", "apps/web", false, "current"},
		{"no-ts conflicts in scope", []string{"apps/web/page.ts"}, Facts{}, "No TypeScript", "apps/web", false, "conflicted"},
		// The regression: a .ts file OUTSIDE the claim's scope must NOT conflict it.
		{"ts in other dir does not conflict apps/web", []string{"docker/fn/index.ts"}, Facts{}, "No TypeScript in the app", "apps/web", false, "current"},
		{"d.ts ignored", []string{"apps/web/types.d.ts"}, Facts{}, "No TypeScript", "apps/web", false, "current"},

		{"neutral fresh", nil, Facts{}, "Prefer small functions", ".", false, "current"},
		{"neutral stale", nil, Facts{}, "Old architecture note", ".", true, "stale"},
	}
	for _, c := range cases {
		status, _, ev := verdict(c.files, c.facts, store.Claim{Text: c.text, Scope: c.scope}, c.stale)
		if status != c.wantStatus {
			t.Errorf("%s: verdict = %q, want %q (evidence: %s)", c.name, status, c.wantStatus, ev)
		}
	}
}

func TestDetectFactsHonorsFileList(t *testing.T) {
	// Facts are derived from the provided (git-tracked) file list only.
	facts := detectFacts([]string{"pnpm-lock.yaml", "cli/go.mod", "apps/www/package.json"}, t.TempDir())
	if facts.PackageManager != "pnpm" {
		t.Errorf("PackageManager = %q, want pnpm", facts.PackageManager)
	}
	if !facts.Go {
		t.Error("expected Go = true from go.mod in file list")
	}
}

func TestDeterministicClaims(t *testing.T) {
	cs := deterministicClaims(Facts{PackageManager: "pnpm", Go: true, CGODisabled: true})
	var hasPnpm, hasCGO bool
	for _, c := range cs {
		if c.Status != "current" || c.Confidence != "high" {
			t.Errorf("deterministic claim not current/high: %+v", c)
		}
		switch c.Text {
		case "Use pnpm for Node package management":
			hasPnpm = true
		case "Keep CGO_ENABLED=0 for static cross-platform binaries":
			hasCGO = true
		}
	}
	if !hasPnpm || !hasCGO {
		t.Errorf("missing deterministic claims: pnpm=%v cgo=%v", hasPnpm, hasCGO)
	}
}
