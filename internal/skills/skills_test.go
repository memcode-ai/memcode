package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSkill(t *testing.T, dir, name, frontmatterBody string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(frontmatterBody), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseFrontmatterForms(t *testing.T) {
	dir := t.TempDir()
	// single-line, quoted, block-scalar description, and a no-name (invalid) file.
	writeSkill(t, dir, "single", "---\nname: single\ndescription: a plain blurb\n---\nBODY-1\n")
	writeSkill(t, dir, "quoted", "---\nname: \"quoted\"\ndescription: 'with quotes'\n---\nBODY-2\n")
	writeSkill(t, dir, "block", "---\nname: block\ndescription: |\n  line one\n  line two\n---\nBODY-3\n")
	writeSkill(t, dir, "nameless", "---\ndescription: no name here\n---\nBODY-4\n")

	got := map[string]Skill{}
	for _, s := range Discover(filepath.Dir(dir) + "/nope") { // force only the .claude/.memcode roots off
		got[s.Name] = s
	}
	// Discover uses fixed roots; scan our temp dir directly instead.
	cands := scanRoot(dir, 0)
	by := map[string]Skill{}
	for _, c := range cands {
		by[c.skill.Name] = c.skill
	}
	if by["single"].Description != "a plain blurb" {
		t.Errorf("single: %q", by["single"].Description)
	}
	if by["quoted"].Name != "quoted" || by["quoted"].Description != "with quotes" {
		t.Errorf("quoted: %+v", by["quoted"])
	}
	if by["block"].Description != "line one line two" {
		t.Errorf("block scalar not joined: %q", by["block"].Description)
	}
	if _, ok := by["nameless"]; ok {
		t.Error("a file without a frontmatter name must not be a skill")
	}
}

func TestNamespacedPluginSkills(t *testing.T) {
	cases := map[string]string{
		"/h/.claude/plugins/cache/claude-plugins-official/vercel/0.43.0/skills/ai-sdk/SKILL.md":                    "vercel:ai-sdk",
		"/h/.claude/plugins/cache/claude-plugins-official/vercel/0.43.0/.claude/skills/release/SKILL.md":           "vercel:release",
		"/h/.claude/plugins/marketplaces/claude-plugins-official/external_plugins/imessage/skills/access/SKILL.md": "imessage:access",
		"/h/.claude/plugins/marketplaces/claude-plugins-official/plugins/hookify/skills/writing-rules/SKILL.md":    "hookify:writing-rules",
		// Non-plugin roots keep the bare name.
		"/h/.memcode/skills/review/SKILL.md":       "review",
		"/h/.claude/skills/deep-research/SKILL.md": "deep-research",
	}
	for path, want := range cases {
		base := strings.TrimSuffix(filepath.Base(filepath.Dir(path)), "")
		if got := namespaced(path, base); got != want {
			t.Errorf("namespaced(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestLoadReturnsBodyAfterFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "x", "---\nname: x\ndescription: d\n---\nthe real body\nmore\n")
	cands := scanRoot(dir, 0)
	if len(cands) != 1 {
		t.Fatalf("want 1 skill, got %d", len(cands))
	}
	body, err := cands[0].skill.Load()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "name: x") || strings.Contains(body, "---") {
		t.Errorf("body should exclude frontmatter: %q", body)
	}
	if !strings.HasPrefix(body, "the real body") {
		t.Errorf("body = %q", body)
	}
}

func TestDiscoverLocalOverridesAndDedup(t *testing.T) {
	repo := t.TempDir()
	// Two skills with the same name in the SAME root: newest mtime wins.
	skdir := filepath.Join(repo, ".memcode", "skills")
	writeSkill(t, skdir, "dup-v1", "---\nname: dup\ndescription: old version\n---\nold\n")
	writeSkill(t, skdir, "dup-v2", "---\nname: dup\ndescription: new version\n---\nnew\n")
	// Make v2 newer.
	older := filepath.Join(skdir, "dup-v1", "SKILL.md")
	newer := filepath.Join(skdir, "dup-v2", "SKILL.md")
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	os.Chtimes(older, old, old)
	os.Chtimes(newer, recent, recent)

	sk := Discover(repo)
	var dup *Skill
	for i := range sk {
		if sk[i].Name == "dup" {
			dup = &sk[i]
		}
	}
	if dup == nil {
		t.Fatal("dedup dropped the skill entirely")
	}
	if dup.Description != "new version" {
		t.Errorf("newest should win, got %q", dup.Description)
	}
	// Exactly one "dup" survives.
	n := 0
	for _, s := range sk {
		if s.Name == "dup" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("dedup failed: %d copies of dup", n)
	}
}

func TestDiscoverNestedAgentsSkills(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // isolate from the real ~/.claude etc.
	// A monorepo subfolder install: apps/www/.agents/skills/supabase/SKILL.md.
	nested := filepath.Join(repo, "apps", "www", ".agents", "skills")
	writeSkill(t, nested, "supabase", "---\nname: supabase\ndescription: Supabase DB/Auth/RLS.\n---\nbody\n")
	// Heavy dirs are pruned, not scanned (a skill buried in node_modules must NOT surface).
	buried := filepath.Join(repo, "node_modules", "pkg", ".agents", "skills")
	writeSkill(t, buried, "should-not-appear", "---\nname: should-not-appear\ndescription: x\n---\nb\n")

	got := map[string]bool{}
	for _, s := range Discover(repo) {
		got[s.Name] = true
	}
	if !got["supabase"] {
		t.Errorf("a nested .agents/skills install must be discovered from the repo root; got %v", got)
	}
	if got["should-not-appear"] {
		t.Error("skills under node_modules must be pruned, not surfaced")
	}
}

func TestScanRootFollowsSymlinks(t *testing.T) {
	// The Agent Skills installers keep the real dir in .agents/skills and SYMLINK it into the
	// agent's native dir. memcode must follow that symlink, not skip it.
	base := t.TempDir()
	real := filepath.Join(base, ".agents", "skills")
	writeSkill(t, real, "find-skills", "---\nname: find-skills\ndescription: discover skills.\n---\nbody\n")

	link := filepath.Join(base, ".claude", "skills")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	cands := scanRoot(link, 0) // scanning the symlinked dir must resolve to the real SKILL.md
	found := false
	for _, c := range cands {
		if c.skill.Name == "find-skills" {
			found = true
		}
	}
	if !found {
		t.Errorf("scanRoot must follow symlinked skill dirs, got %d candidates", len(cands))
	}
}

func names(sk []Skill) []string {
	var n []string
	for _, s := range sk {
		n = append(n, s.Name)
	}
	return n
}

func TestSearch(t *testing.T) {
	sk := []Skill{
		{Name: "supabase", Description: "Supabase Database, Auth, RLS, Edge Functions, Realtime."},
		{Name: "vercel:nextjs", Description: "Next.js App Router, deployment on Vercel."},
		{Name: "claude-api", Description: "Anthropic Claude API, pricing, tool use."},
	}
	// A technology query surfaces the matching skill, ranked first.
	got := Search(sk, "supabase", 6)
	if len(got) == 0 || got[0].Name != "supabase" {
		t.Fatalf("query 'supabase' should rank the supabase skill first, got %v", names(got))
	}
	// A token in the description matches even without the name.
	if got := Search(sk, "rls auth", 6); len(got) == 0 || got[0].Name != "supabase" {
		t.Errorf("description tokens should match: %v", names(got))
	}
	// No match → empty (so the tool can say "none").
	if got := Search(sk, "kubernetes", 6); len(got) != 0 {
		t.Errorf("an unmatched query should return nothing, got %v", names(got))
	}
	// Empty query → nothing.
	if got := Search(sk, "  ", 6); len(got) != 0 {
		t.Errorf("blank query should return nothing, got %v", names(got))
	}
}

func TestRootsExistingOnly(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // isolate from the real ~/.claude on this machine
	// No skill dirs exist yet → no roots.
	if got := Roots(repo); len(got) != 0 {
		t.Errorf("no skill dirs should mean no roots, got %v", got)
	}
	// Create the project-local skills dir → it shows up.
	skdir := filepath.Join(repo, ".memcode", "skills")
	if err := os.MkdirAll(skdir, 0o755); err != nil {
		t.Fatal(err)
	}
	got := Roots(repo)
	found := false
	for _, d := range got {
		if d == skdir {
			found = true
		}
	}
	if !found {
		t.Errorf("an existing .memcode/skills dir should be a root, got %v", got)
	}
}
