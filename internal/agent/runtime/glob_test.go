package runtime

import "testing"

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"**/*.md", "README.md", true},
		{"**/*.md", "infra/RUNPOD.md", true},
		{"**/*.md", "a/b/c.md", true},
		{"**/*.md", "main.go", false},
		{"*.md", "README.md", true},
		{"*.md", "infra/RUNPOD.md", false}, // no ** → root only
		{"internal/**/*_test.go", "internal/agent/runtime/glob_test.go", true},
		{"internal/**/*_test.go", "cmd/arch.go", false},
	}
	for _, c := range cases {
		re, err := globToRegexp(c.pat)
		if err != nil {
			t.Fatalf("compile %q: %v", c.pat, err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("glob %q vs %q = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestHasHiddenSegment(t *testing.T) {
	hidden := []string{".memcode/sessions/x.md", ".git/config", "a/.claude/p.md", ".github/workflows/ci.yml"}
	visible := []string{"README.md", "cli/internal/stack/stack.go", "infra/RUNPOD.md"}
	for _, p := range hidden {
		if !hasHiddenSegment(p) {
			t.Errorf("%q should be hidden", p)
		}
	}
	for _, p := range visible {
		if hasHiddenSegment(p) {
			t.Errorf("%q should be visible", p)
		}
	}
}
