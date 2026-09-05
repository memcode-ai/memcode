// Package skills gives memcode discoverable, lazy-loaded "traits" — the same
// mechanism Claude Code uses: a markdown file with a name + a "when to use" blurb,
// its body loaded on demand when the model decides a task matches. The system prompt
// only POINTS at the skill dirs (see Roots) — the model greps/reads them when a task
// fits, rather than every skill's blurb being dumped into context.
//
// Discovery is UNIVERSAL (see discoveryRoots): it doesn't matter whether a skill was
// installed user-global, at the repo root, or in a monorepo subfolder, nor whether its dir
// is a real one or a symlink an installer dropped. memcode unifies its own skills with the
// cross-agent Agent Skills standard (.agents/skills, what the `skills` CLI installs) and
// Claude Code's dirs — so claude-api, vercel:*, supabase, etc. all come along for free.
//
// The trust boundary is "installed": a skill only exists because someone put a
// file in one of these roots (or installed a plugin), so loading its body is safe.
package skills

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is one discovered trait: its frontmatter (the Agent Skills open spec,
// agentskills.io) plus where its body lives. Required spec fields: Name, Description.
// Optional spec fields: License, Compatibility, AllowedTools (experimental), and a free-form
// Metadata map (where the spec parks Version, author, argument-hint, …).
type Skill struct {
	Name          string            // unique slug — plugin skills are namespaced (e.g. "vercel:ai-sdk")
	Description   string            // the "when to use" blurb shown in the catalog
	License       string            // SPDX id, optional (spec)
	Compatibility string            // declared agent/runtime compatibility, optional (spec)
	AllowedTools  string            // experimental tool scope, e.g. "Bash(git:*) Read" (spec)
	Version       string            // metadata.version, optional (spec parks version under metadata)
	Metadata      map[string]string // the full metadata map (author, version, argument-hint, …)
	Path          string            // the SKILL.md / *.md file holding the body
	Dir           string            // the skill's directory (bundled scripts/files the body may reference)
}

type candidate struct {
	skill    Skill
	priority int    // lower = preferred source (local overrides installed)
	version  string // declared metadata.version — the truest "which is latest" signal
	mtimeNs  int64  // fallback tiebreak when versions are equal/absent
}

// rootSpec is one discovery location plus its precedence (lower = preferred when the same
// skill name turns up in several places).
type rootSpec struct {
	dir      string
	priority int
}

// discoveryRoots is memcode's UNIVERSAL skill map — every place a skill conventionally
// lives, unified into one list, so it doesn't matter whether a skill was installed
// user-global, at the repo root, or in a monorepo subfolder, nor whether its dir is real or
// a symlink an installer dropped. It covers:
//   - memcode's own:         <repo>/.memcode/skills, ~/.memcode/skills
//   - the Agent Skills std:  .agents/skills (user-global AND anywhere in the repo)
//   - Claude Code:           .claude/skills (anywhere in the repo) + user ~/.claude/{skills,plugins}
//
// Repo-local dirs (including nested ones) outrank caller-supplied extra roots
// (a gateway agent's own skills — more specific than the user), which outrank
// user-global ones.
func discoveryRoots(repoRoot string, extra []string) []rootSpec {
	roots := []rootSpec{{filepath.Join(repoRoot, ".memcode", "skills"), 0}}
	for _, d := range nestedSkillDirs(repoRoot) { // .agents/skills + .claude/skills, repo-root or nested
		roots = append(roots, rootSpec{d, 1})
	}
	for _, d := range extra {
		roots = append(roots, rootSpec{d, 2})
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots,
			rootSpec{filepath.Join(home, ".agents", "skills"), 3},  // Agent Skills, user-global
			rootSpec{filepath.Join(home, ".memcode", "skills"), 3}, // memcode, user-global
			rootSpec{filepath.Join(home, ".claude", "skills"), 4},  // Claude Code user skills
			rootSpec{filepath.Join(home, ".claude", "plugins"), 5}, // Claude Code plugins
		)
	}
	return dedupRoots(roots)
}

// dedupRoots drops repeated directories (a nested-walk hit can coincide with a fixed root),
// keeping the first (highest-precedence) occurrence.
func dedupRoots(in []rootSpec) []rootSpec {
	seen := map[string]bool{}
	var out []rootSpec
	for _, r := range in {
		c := filepath.Clean(r.dir)
		if c == "" || c == "." || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, rootSpec{c, r.priority})
	}
	return out
}

// prunedDirs are never descended into when hunting for skill dirs — big, generated, or
// irrelevant trees (.git can be huge; node_modules carries package-internal skills we don't
// want to surface as the user's).
var prunedDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	"out": true, "target": true, ".next": true, ".turbo": true, ".cache": true, ".venv": true,
}

// The nested walk is BOUNDED. A skill dir lives a few levels down in a monorepo,
// never twelve; and the root handed to us is not always a repo — a stray root
// resolution once pointed this walk at the user's entire home directory and cost
// tens of seconds on every launch, before the prompt appeared. Real installs fit
// far under these caps; anything that trips them is a tree we shouldn't be
// crawling for skills anyway.
const (
	walkMaxDepth = 8
	walkMaxDirs  = 20000
)

// nestedSkillDirs finds every .agents/skills and .claude/skills directory ANYWHERE in the
// repo — so a monorepo subfolder install (e.g. apps/www/.agents/skills) is discovered from
// the repo root — while pruning heavy trees so the walk stays cheap.
func nestedSkillDirs(repoRoot string) []string {
	var out []string
	seen := 0
	_ = filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr
		}
		if prunedDirs[d.Name()] {
			return filepath.SkipDir
		}
		if seen++; seen > walkMaxDirs {
			return filepath.SkipAll
		}
		if rel, e := filepath.Rel(repoRoot, path); e == nil &&
			rel != "." && strings.Count(filepath.ToSlash(rel), "/")+1 >= walkMaxDepth {
			return filepath.SkipDir
		}
		if d.Name() == "skills" {
			if p := filepath.Base(filepath.Dir(path)); p == ".agents" || p == ".claude" {
				out = append(out, path)
			}
		}
		return nil
	})
	return out
}

// Discover scans the universal roots and returns one skill per name, with repo-local skills
// overriding user-global ones and, within a source, the newest file winning.
func Discover(repoRoot string) []Skill { return DiscoverIn(repoRoot, nil) }

// DiscoverIn is Discover plus caller-supplied extra roots (e.g. a gateway
// agent's own skills dir), which rank between repo-local and user-global.
func DiscoverIn(repoRoot string, extraRoots []string) []Skill {
	var cands []candidate
	for _, r := range discoveryRoots(repoRoot, extraRoots) {
		cands = append(cands, scanRoot(r.dir, r.priority)...)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].priority != cands[j].priority {
			return cands[i].priority < cands[j].priority
		}
		if v := compareVersions(cands[i].version, cands[j].version); v != 0 {
			return v > 0 // higher DECLARED version wins (metadata.version — truer than mtime)
		}
		return cands[i].mtimeNs > cands[j].mtimeNs // fallback: newest file
	})
	seen := map[string]bool{}
	var out []Skill
	for _, c := range cands {
		if c.skill.Name == "" || seen[c.skill.Name] {
			continue
		}
		seen[c.skill.Name] = true
		out = append(out, c.skill)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// scanRoot scans one root for skill files (SKILL.md, or any *.md carrying skill
// frontmatter), FOLLOWING SYMLINKS — the Agent Skills installers keep a canonical copy in
// .agents/skills and symlink it into each agent's native dir, so the symlinked dirs must be
// resolved, not skipped (which filepath.WalkDir does for symlinks).
func scanRoot(root string, priority int) []candidate {
	return scanDir(root, priority, 0)
}

// scanDir recurses a directory, resolving symlinks via os.Stat. Depth-capped so a symlink
// cycle can't loop forever.
func scanDir(dir string, priority, depth int) []candidate {
	if depth > 12 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []candidate
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		fi, err := os.Stat(full) // follows symlinks (DirEntry.IsDir would report a link as a non-dir)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			out = append(out, scanDir(full, priority, depth+1)...)
			continue
		}
		base := e.Name()
		if base != "SKILL.md" && !strings.HasSuffix(base, ".md") {
			continue
		}
		sk, ok := parseFrontmatter(full)
		if !ok {
			continue
		}
		sk.Name = namespaced(full, sk.Name)
		sk.Path = full
		sk.Dir = filepath.Dir(full)
		out = append(out, candidate{
			skill:    sk,
			priority: priority, version: sk.Version, mtimeNs: fi.ModTime().UnixNano(),
		})
	}
	return out
}

// namespaced prefixes a plugin skill with its plugin (vercel:ai-sdk), matching how
// the host identifies them and preventing cross-plugin name collisions. Non-plugin
// skills (project/user) keep their bare name. The plugin dir sits just above the
// `skills` segment, but a version dir and/or a `.claude` dir can sit between them,
// and the layout varies (cache vs marketplaces) — so we walk back from `skills`,
// skipping version-like and `.claude` segments, and take the first real name:
//
//	…/vercel/0.43.0/skills/ai-sdk/SKILL.md           → vercel
//	…/vercel/0.43.0/.claude/skills/release/SKILL.md  → vercel
//	…/external_plugins/imessage/skills/access/…      → imessage
func namespaced(path, name string) string {
	slash := filepath.ToSlash(path)
	if !strings.Contains(slash, "/plugins/") {
		return name
	}
	parts := strings.Split(slash, "/")
	skillsIdx := -1
	for i, p := range parts {
		if p == "skills" {
			skillsIdx = i // last "skills" wins
		}
	}
	j := skillsIdx - 1
	for j >= 0 && (parts[j] == ".claude" || looksLikeVersion(parts[j])) {
		j--
	}
	if j >= 0 && parts[j] != "" {
		return parts[j] + ":" + name
	}
	return name
}

// looksLikeVersion reports whether a path segment is a version dir (e.g. "0.43.0").
func looksLikeVersion(s string) bool {
	return len(s) > 0 && s[0] >= '0' && s[0] <= '9' && strings.Contains(s, ".")
}

// parseFrontmatter reads the leading `---`-delimited YAML frontmatter and pulls out the Agent
// Skills spec fields (agentskills.io): required name + description, optional license,
// compatibility, allowed-tools, and a nested metadata map (where the spec parks version,
// author, …). Hand-parsed in this package's lightweight style — no YAML dependency. A file
// without a frontmatter name isn't a skill (returns ok=false). Path/Dir are filled by the caller.
func parseFrontmatter(path string) (Skill, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, false
	}
	body := string(b)
	if !strings.HasPrefix(body, "---") {
		return Skill{}, false
	}
	// Frontmatter is between the first two `---` lines.
	rest := strings.TrimPrefix(body, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return Skill{}, false
	}
	lines := strings.Split(rest[:end], "\n")
	var sk Skill
	for i := 0; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], " ") || strings.HasPrefix(lines[i], "\t") {
			continue // indented — consumed by its parent key (description block / metadata map)
		}
		k, v, found := strings.Cut(lines[i], ":")
		if !found {
			continue
		}
		val := strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "name":
			sk.Name = unquote(val)
		case "description":
			sk.Description = blockOrScalar(val, lines, &i)
		case "license":
			sk.License = unquote(val)
		case "compatibility":
			sk.Compatibility = unquote(val)
		case "allowed-tools":
			sk.AllowedTools = unquote(val)
		case "metadata":
			sk.Metadata = indentedMap(lines, &i)
			sk.Version = sk.Metadata["version"]
		}
	}
	if sk.Name == "" {
		return Skill{}, false
	}
	return sk, true
}

// blockOrScalar returns a frontmatter value that may be inline or a YAML block scalar
// ("|" / ">" / empty → the real text is on the following indented lines). Advances *i past any
// continuation lines consumed.
func blockOrScalar(val string, lines []string, i *int) string {
	if val != "" && val != "|" && val != ">" && val != "|-" && val != ">-" {
		return unquote(val)
	}
	var parts []string
	for j := *i + 1; j < len(lines) && (strings.HasPrefix(lines[j], "  ") || strings.TrimSpace(lines[j]) == ""); j++ {
		if t := strings.TrimSpace(lines[j]); t != "" {
			parts = append(parts, t)
		}
		*i = j
	}
	return strings.Join(parts, " ")
}

// indentedMap reads the indented `key: value` lines following a parent key (e.g. `metadata:`)
// into a map, advancing *i past them and stopping at the first dedent (next top-level key).
func indentedMap(lines []string, i *int) map[string]string {
	m := map[string]string{}
	for j := *i + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			*i = j
			continue
		}
		if !strings.HasPrefix(lines[j], " ") && !strings.HasPrefix(lines[j], "\t") {
			break // dedent → end of the nested block
		}
		if k, v, found := strings.Cut(strings.TrimSpace(lines[j]), ":"); found {
			m[strings.TrimSpace(k)] = unquote(strings.TrimSpace(v))
		}
		*i = j
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// Validate reports a skill's deviations from the Agent Skills spec (agentskills.io) — empty
// means conformant. memcode discovers LENIENTLY (a non-conformant skill still loads), so this
// surfaces conformance for auditing/warnings; it does not gate discovery.
func (s Skill) Validate() []string {
	var issues []string
	base := bareName(s.Name) // strip any plugin namespace before the format/dir checks
	switch {
	case base == "":
		issues = append(issues, "missing required `name`")
	default:
		if len(base) > 64 {
			issues = append(issues, "name exceeds 64 chars")
		}
		if !isSlug(base) {
			issues = append(issues, "name must be lowercase letters, digits, and hyphens")
		}
		if s.Dir != "" && filepath.Base(s.Dir) != base {
			issues = append(issues, "name must equal the skill's directory name ("+filepath.Base(s.Dir)+")")
		}
	}
	if d := strings.TrimSpace(s.Description); d == "" {
		issues = append(issues, "missing required `description`")
	} else if len(d) > 1024 {
		issues = append(issues, "description exceeds 1024 chars")
	}
	return issues
}

// bareName strips a plugin namespace ("vercel:ai-sdk" → "ai-sdk") for the spec's name checks.
func bareName(n string) string {
	if i := strings.IndexByte(n, ':'); i >= 0 {
		return n[i+1:]
	}
	return n
}

// isSlug reports whether s is the spec's name charset: lowercase letters, digits, hyphens.
func isSlug(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return s != ""
}

// compareVersions is a best-effort numeric dotted compare ("3.0.0" > "1.2.0"); non-numeric
// segments count as 0. Returns -1, 0, or 1.
func compareVersions(a, b string) int {
	if a == b {
		return 0
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x = atoiSafe(as[i])
		}
		if i < len(bs) {
			y = atoiSafe(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// Search ranks installed skills against a query — a technology / library / CLI / topic the
// agent is about to work with (e.g. "supabase", "next.js", "stripe") — and returns the best
// matches (at most max). Unlike the old always-on catalog, this runs ONLY when the model
// explicitly asks (the `skill` find tool), so there's no per-turn context cost and no
// brittle auto-gating: the agent supplies the query when it recognizes a 3rd-party tool.
// A whole-query substring hit scores strongest; individual query tokens add to it.
func Search(sk []Skill, query string, max int) []Skill {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" || len(sk) == 0 || max <= 0 {
		return nil
	}
	var qtoks []string
	for _, t := range strings.FieldsFunc(q, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(t) >= 3 {
			qtoks = append(qtoks, t)
		}
	}
	type scored struct {
		s Skill
		n int
	}
	var hits []scored
	for _, s := range sk {
		hay := strings.ToLower(s.Name + " " + s.Description)
		n := 0
		if strings.Contains(hay, q) {
			n += 5 // whole-query match is a strong signal
		}
		for _, t := range qtoks {
			if strings.Contains(hay, t) {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, scored{s, n})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].s.Name < hits[j].s.Name
	})
	if len(hits) > max {
		hits = hits[:max]
	}
	out := make([]Skill, len(hits))
	for i, h := range hits {
		out[i] = h.s
	}
	return out
}

// Load returns the skill's body — everything after the frontmatter block.
func (s Skill) Load() (string, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	body := string(b)
	if strings.HasPrefix(body, "---") {
		rest := strings.TrimPrefix(body, "---")
		if i := strings.Index(rest, "\n---"); i >= 0 {
			after := rest[i+len("\n---"):]
			if nl := strings.IndexByte(after, '\n'); nl >= 0 {
				return strings.TrimLeft(after[nl+1:], "\n"), nil
			}
		}
	}
	return body, nil
}

// Roots returns the EXISTING skill source directories, in precedence order — the universal
// set (see discoveryRoots): memcode's own, the Agent Skills .agents/skills convention
// (user-global + anywhere in the repo), and Claude Code's dirs. Pointed at from the system
// prompt so the model can grep/read them on demand instead of having every skill's blurb
// dumped into context.
func Roots(repoRoot string) []string { return RootsIn(repoRoot, nil) }

// RootsIn is Roots plus caller-supplied extra roots (see DiscoverIn).
func RootsIn(repoRoot string, extraRoots []string) []string {
	var out []string
	for _, r := range discoveryRoots(repoRoot, extraRoots) {
		if fi, err := os.Stat(r.dir); err == nil && fi.IsDir() {
			out = append(out, r.dir)
		}
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
