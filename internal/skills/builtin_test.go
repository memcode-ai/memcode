package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinWebsitesDiscoveredWithoutFilesystemRoots(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "missing-repo")
	home := filepath.Join(t.TempDir(), "missing-home")
	t.Setenv("HOME", home)

	var got *Skill
	for _, sk := range DiscoverIn(repo, nil) {
		if sk.Name == "websites" {
			s := sk
			got = &s
			break
		}
	}
	if got == nil {
		t.Fatal("built-in websites skill was not discovered")
	}
	if got.Path != "built-in/websites/SKILL.md" || got.Dir != "built-in/websites" {
		t.Fatalf("built-in identity = Path %q Dir %q", got.Path, got.Dir)
	}
	body, err := got.Load()
	if err != nil {
		t.Fatalf("embedded Load(): %v", err)
	}
	if strings.TrimSpace(body) == "" || strings.Contains(body, "name: websites") {
		t.Fatalf("embedded Load() returned wrong body: %q", body)
	}
}

func TestBuiltinWebsitesRepoLocalOverrideWins(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("HOME", filepath.Join(t.TempDir(), "missing-home"))
	writeSkill(t, filepath.Join(repo, ".memcode", "skills"), "websites", `---
name: websites
description: repo override
metadata:
  version: "99"
---
repo body
`)

	var got *Skill
	for _, sk := range DiscoverIn(repo, nil) {
		if sk.Name == "websites" {
			s := sk
			got = &s
			break
		}
	}
	if got == nil {
		t.Fatal("websites skill missing")
	}
	if got.Description != "repo override" {
		t.Fatalf("repo override did not win: %+v", got)
	}
	body, err := got.Load()
	if err != nil || strings.TrimSpace(body) != "repo body" {
		t.Fatalf("Load() = %q, %v", body, err)
	}
}

func TestBuiltinWebsitesUserGlobalOverrideWins(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "missing-repo")
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSkill(t, filepath.Join(home, ".memcode", "skills"), "websites", `---
name: websites
description: user override
metadata:
  version: "2"
---
user body
`)

	var got *Skill
	for _, sk := range DiscoverIn(repo, nil) {
		if sk.Name == "websites" {
			s := sk
			got = &s
			break
		}
	}
	if got == nil {
		t.Fatal("websites skill missing")
	}
	if got.Description != "user override" {
		t.Fatalf("user override did not win: %+v", got)
	}
}

func TestBuiltinWebsitesSkillValidates(t *testing.T) {
	for _, sk := range builtinCandidates() {
		if sk.skill.Name != "websites" {
			continue
		}
		if issues := sk.skill.Validate(); len(issues) != 0 {
			t.Fatalf("built-in websites skill must conform: %v", issues)
		}
		return
	}
	t.Fatal("built-in websites candidate missing")
}

func TestBuiltinLoadNeverReadsDeclaredPath(t *testing.T) {
	// Proves Load's `body` branch: the synthetic path cannot exist, yet
	// Load succeeds because its body came from go:embed at discovery time.
	for _, c := range builtinCandidates() {
		if c.skill.Name != "websites" {
			continue
		}
		if _, err := os.Stat(c.skill.Path); !os.IsNotExist(err) {
			t.Fatalf("test expects non-filesystem builtin path %q, stat err %v", c.skill.Path, err)
		}
		if body, err := c.skill.Load(); err != nil || body == "" {
			t.Fatalf("Load from embedded body = %q, %v", body, err)
		}
		return
	}
	t.Fatal("built-in websites candidate missing")
}
