package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realDeployToVercel is the ACTUAL frontmatter of vercel-labs/agent-skills@deploy-to-vercel as
// installed by `npx skills add` from skills.sh (captured verbatim) — so this test proves memcode
// parses a real ecosystem skill, not a hand-made fixture.
const realDeployToVercel = `---
name: deploy-to-vercel
description: Deploy applications and websites to Vercel. Use when the user requests deployment actions like "deploy my app", "deploy and give me the link", "push this live", or "create a preview deployment".
metadata:
  author: vercel
  version: "3.0.0"
---

# Deploy to Vercel

Use the Vercel CLI to deploy.
`

// A real skills.sh skill must parse with every spec field we now support: required name +
// description, and the nested metadata map (where the spec parks version/author).
func TestParseRealSkillshSkill(t *testing.T) {
	p := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(p, []byte(realDeployToVercel), 0o644); err != nil {
		t.Fatal(err)
	}
	sk, ok := parseFrontmatter(p)
	if !ok {
		t.Fatal("real skills.sh SKILL.md failed to parse")
	}
	if sk.Name != "deploy-to-vercel" {
		t.Errorf("name = %q", sk.Name)
	}
	if !strings.HasPrefix(sk.Description, "Deploy applications") {
		t.Errorf("description = %q", sk.Description)
	}
	if sk.Version != "3.0.0" {
		t.Errorf("version = %q, want 3.0.0 (from metadata.version — was dropped before)", sk.Version)
	}
	if sk.Metadata["author"] != "vercel" {
		t.Errorf("metadata.author = %q, want vercel", sk.Metadata["author"])
	}
}

// End-to-end: a skill installed where `skills add` puts it (.agents/skills) is discovered by
// memcode, with its declared version carried through. This is the integration proof — the
// skills.sh install location IS a memcode discovery root.
func TestDiscoverAgentsSkillsInstall(t *testing.T) {
	repo := t.TempDir()
	writeSkill(t, filepath.Join(repo, ".agents", "skills"), "deploy-to-vercel", realDeployToVercel)

	var got *Skill
	for i := range Discover(repo) {
		if Discover(repo)[i].Name == "deploy-to-vercel" {
			s := Discover(repo)[i]
			got = &s
			break
		}
	}
	if got == nil {
		t.Fatal("a skill installed under .agents/skills (where skills.sh installs) was not discovered")
	}
	if got.Version != "3.0.0" {
		t.Errorf("discovered version = %q, want 3.0.0", got.Version)
	}
	if issues := got.Validate(); len(issues) != 0 {
		t.Errorf("a conformant real skill should validate clean, got %v", issues)
	}
}

// The optional spec fields (license, compatibility, allowed-tools) and a description block
// scalar all parse.
func TestParseFullSpecFields(t *testing.T) {
	const content = `---
name: my-skill
description: |
  A multi-line description
  spread over two lines.
license: MIT
compatibility: ">=1.0"
allowed-tools: Bash(git:*) Read
metadata:
  version: "2.5.1"
---
body
`
	p := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sk, ok := parseFrontmatter(p)
	if !ok {
		t.Fatal("parse failed")
	}
	if sk.Description != "A multi-line description spread over two lines." {
		t.Errorf("block-scalar description = %q", sk.Description)
	}
	if sk.License != "MIT" || sk.Compatibility != ">=1.0" || sk.AllowedTools != "Bash(git:*) Read" {
		t.Errorf("optional fields: license=%q compat=%q tools=%q", sk.License, sk.Compatibility, sk.AllowedTools)
	}
	if sk.Version != "2.5.1" {
		t.Errorf("version = %q", sk.Version)
	}
}

// Validate flags spec deviations (uppercase name, name != dir) but stays empty for conformant.
func TestValidate(t *testing.T) {
	ok := Skill{Name: "good-skill", Description: "does a thing", Dir: "/x/good-skill"}
	if issues := ok.Validate(); len(issues) != 0 {
		t.Errorf("conformant skill flagged: %v", issues)
	}
	bad := Skill{Name: "Bad_Name", Description: "", Dir: "/x/other-dir"}
	issues := strings.Join(bad.Validate(), " | ")
	for _, want := range []string{"lowercase", "directory name", "description"} {
		if !strings.Contains(issues, want) {
			t.Errorf("expected issue mentioning %q, got: %s", want, issues)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"3.0.0", "1.2.0", 1},
		{"1.2.0", "1.10.0", -1}, // numeric, not lexical
		{"2.5.1", "2.5.1", 0},
		{"1.0", "1.0.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// parseFind reads real `skills find` output (ANSI stripped) into structured hits, including
// packages whose skill segment contains a colon (google-labs-code/stitch-skills@react:components).
func TestParseFind(t *testing.T) {
	const out = `
Install with npx skills add <owner/repo@skill>

vercel-labs/agent-skills@vercel-react-best-practices 509.5K installs
└ https://skills.sh/vercel-labs/agent-skills/vercel-react-best-practices

google-labs-code/stitch-skills@react:components 50.4K installs
└ https://skills.sh/google-labs-code/stitch-skills/react:components
`
	hits := parseFind(out)
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2: %+v", len(hits), hits)
	}
	if hits[0].Package != "vercel-labs/agent-skills@vercel-react-best-practices" || hits[0].Installs != "509.5K" {
		t.Errorf("hit0 = %+v", hits[0])
	}
	if !strings.HasPrefix(hits[0].URL, "https://skills.sh/") {
		t.Errorf("hit0 url = %q", hits[0].URL)
	}
	if hits[1].Package != "google-labs-code/stitch-skills@react:components" {
		t.Errorf("hit1 package = %q (colon in skill segment must survive)", hits[1].Package)
	}
}

// TestLiveSkillshRoundtrip exercises memcode's REAL code paths against the LIVE skills.sh
// service: search → install → discover → load. Env-gated (needs network + npx), so it doesn't
// run in CI, but it's the end-to-end proof that a skill from skills.sh is usable in memcode.
func TestLiveSkillshRoundtrip(t *testing.T) {
	if os.Getenv("MEMCODE_LIVE_SKILLS") == "" {
		t.Skip("set MEMCODE_LIVE_SKILLS=1 to run the live skills.sh roundtrip (needs network + npx)")
	}
	ctx := context.Background()

	hits, err := RemoteFind(ctx, "react", "")
	if err != nil {
		t.Fatalf("RemoteFind: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("RemoteFind('react') returned no catalog hits")
	}
	t.Logf("RemoteFind('react') → %d hits; first = %s (%s installs)", len(hits), hits[0].Package, hits[0].Installs)

	repo := t.TempDir()
	out, err := RemoteAdd(ctx, repo, "vercel-labs/agent-skills@deploy-to-vercel")
	if err != nil {
		t.Fatalf("RemoteAdd: %v", err)
	}
	t.Logf("RemoteAdd installed into %s", repo)
	_ = out

	var found *Skill
	for _, s := range Discover(repo) {
		if s.Name == "deploy-to-vercel" {
			ss := s
			found = &ss
			break
		}
	}
	if found == nil {
		t.Fatal("installed skill was NOT discovered by memcode after RemoteAdd")
	}
	body, err := found.Load()
	if err != nil || strings.TrimSpace(body) == "" {
		t.Fatalf("Load() of the installed skill failed: err=%v body=%q", err, body)
	}
	t.Logf("✓ discovered + loaded %q (%d-byte body, version %q) — skills.sh skill usable in memcode",
		found.Name, len(body), found.Version)
}
