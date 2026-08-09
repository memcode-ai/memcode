package runtime

import (
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/skills"
)

func TestSkillNudge(t *testing.T) {
	newSess := func(names ...string) *Session {
		sk := make([]skills.Skill, len(names))
		for i, n := range names {
			sk[i] = skills.Skill{Name: n}
		}
		return &Session{skills: sk, nudgedSkills: map[string]bool{}}
	}

	// A request that names an installed skill gets a nudge naming that skill.
	s := newSess("supabase", "vercel:ai-sdk")
	if n := s.skillNudge("can you update the supabase schema"); !strings.Contains(n, "supabase") {
		t.Fatalf("expected supabase nudge, got %q", n)
	}
	// Fires once per trigger per session — no nagging on every turn.
	if again := s.skillNudge("more supabase work"); again != "" {
		t.Fatalf("expected at-most-once nudge, got %q", again)
	}

	// Namespaced skill matches on its plugin namespace (vercel:ai-sdk → "vercel").
	if v := newSess("vercel:ai-sdk").skillNudge("deploy this with vercel"); !strings.Contains(v, "vercel") {
		t.Fatalf("expected vercel nudge, got %q", v)
	}

	// No installed skill named → no nudge.
	if z := newSess("supabase").skillNudge("just refactor this function"); z != "" {
		t.Fatalf("expected no nudge, got %q", z)
	}

	// Substring is NOT a match — word-boundary only (so "subscription" won't trip "sub").
	if z := newSess("supabase").skillNudge("manage the subscription"); z != "" {
		t.Fatalf("expected no substring match, got %q", z)
	}

	// Triggers shorter than 4 chars are ignored (too noisy: fly, gh, …).
	if f := newSess("fly").skillNudge("fly to the moon"); f != "" {
		t.Fatalf("expected short trigger to be ignored, got %q", f)
	}
}
