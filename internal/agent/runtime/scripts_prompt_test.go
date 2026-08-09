package runtime

import (
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/scripts"
)

func TestScriptsPointer(t *testing.T) {
	if p := scriptsPointer(nil); p != "" {
		t.Fatalf("expected no pointer when nothing is saved, got %q", p)
	}
	p := scriptsPointer([]scripts.Script{{Slug: "rebuild-cli"}})
	if !strings.Contains(p, "script") {
		t.Fatalf("expected the pointer to mention the script tool, got %q", p)
	}
}

func TestScriptNudge(t *testing.T) {
	newSess := func(slugs ...string) *Session {
		sc := make([]scripts.Script, len(slugs))
		for i, sl := range slugs {
			sc[i] = scripts.Script{Slug: sl}
		}
		return &Session{scripts: sc, nudgedScripts: map[string]bool{}}
	}

	// A request that names a saved script's slug (all hyphen-parts present) gets a nudge.
	s := newSess("rebuild-cli", "commit-push-deploy")
	if n := s.scriptNudge("please rebuild the cli again"); !strings.Contains(n, "rebuild-cli") {
		t.Fatalf("expected rebuild-cli nudge, got %q", n)
	}
	// Fires once per slug per session — no nagging on every turn.
	if again := s.scriptNudge("rebuild the cli once more"); again != "" {
		t.Fatalf("expected at-most-once nudge, got %q", again)
	}

	// A partial match (missing one of the slug's words) is NOT a match — high precision.
	if z := newSess("rebuild-cli").scriptNudge("just rebuild it"); z != "" {
		t.Fatalf("expected no nudge on a partial word match, got %q", z)
	}

	// No saved script named → no nudge.
	if z := newSess("rebuild-cli").scriptNudge("refactor this function"); z != "" {
		t.Fatalf("expected no nudge, got %q", z)
	}

	// No saved scripts at all → no nudge, no panic.
	if z := (&Session{nudgedScripts: map[string]bool{}}).scriptNudge("rebuild the cli"); z != "" {
		t.Fatalf("expected no nudge with an empty catalog, got %q", z)
	}
}
