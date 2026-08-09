// Package knowledge gives memcode the baseline a senior engineer just HAS — curated,
// authoritative facts and idioms for common stacks (Vercel, Next, React, Node, Supabase…),
// embedded in the binary so they travel into any repo.
//
// Why this exists: the routing models have weaker/staler recall of fast-moving framework
// specifics than a frontier model. A dogfood turn invented a custom NEXT_PUBLIC_COMING_SOON
// env var instead of Vercel's built-in VERCEL_ENV. The packs give the model an authoritative,
// ungated reference to consult — surfaced via a repo-detected pointer (deterministic FACT) and
// the `knowledge` tool (model judgment). We deliberately do NOT keyword-match the user's prose
// to force content in — that's CLI-side intent classification, which doctrine keeps out of the CLI.
//
// Two kinds of content, deliberately separated (see the pack format):
//   - Facts  — authoritative, unconditional. Overrides the model's priors.
//   - Idioms — greenfield defaults that DEFER to existing code (never refactor working code to
//     satisfy them).
package knowledge

import (
	"embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed packs/*.md
var packFS embed.FS

// Pack is one stack's curated knowledge, parsed from packs/<name>.md.
type Pack struct {
	Name     string   // canonical slug, e.g. "vercel"
	Triggers []string // words in the user's text that surface this pack, e.g. ["vercel"]
	Detect   Hints    // repo-detection hints (files present / manifest deps)
	Facts    string   // authoritative, unconditional — the injected digest
	Idioms   string   // greenfield defaults (consult-only; defer to existing code)
}

// Hints lists the cheap, deterministic signals that a repo uses this stack.
type Hints struct {
	Files []string // files whose presence implies the stack, e.g. "vercel.json"
	Deps  []string // dependency names to look for in package.json / go.mod
}

var (
	catalogOnce sync.Once
	catalog     []Pack
)

// Catalog returns every embedded pack, parsed once and sorted by name.
func Catalog() []Pack {
	catalogOnce.Do(func() {
		entries, err := packFS.ReadDir("packs")
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			b, err := packFS.ReadFile("packs/" + e.Name())
			if err != nil {
				continue
			}
			if p, ok := parse(string(b)); ok {
				catalog = append(catalog, p)
			}
		}
		sort.Slice(catalog, func(i, j int) bool { return catalog[i].Name < catalog[j].Name })
	})
	return catalog
}

// Detect returns the packs that fingerprint the repo at root — a file they name exists, or a
// dependency they name appears in package.json / go.mod. Deterministic and cheap; the net for
// turns where the user DIDN'T name the stack but is clearly working in it.
func Detect(root string) []Pack {
	deps := manifestDeps(root)
	var out []Pack
	for _, p := range Catalog() {
		if p.matchesRepo(root, deps) {
			out = append(out, p)
		}
	}
	return out
}

func (p Pack) matchesRepo(root string, deps map[string]bool) bool {
	for _, f := range p.Detect.Files {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			return true
		}
	}
	for _, d := range p.Detect.Deps {
		if deps[strings.ToLower(d)] {
			return true
		}
	}
	return false
}

// Find ranks packs against a free-text query for the tool's `find` mode — a substring hit on
// the name or any trigger. Simple by design; the catalog is small and curated.
func Find(query string) []Pack {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var out []Pack
	for _, p := range Catalog() {
		hay := p.Name + " " + strings.Join(p.Triggers, " ")
		if strings.Contains(hay, q) || strings.Contains(q, p.Name) {
			out = append(out, p)
		}
	}
	return out
}

// Get resolves a pack by exact (case-insensitive) name.
func Get(name string) (Pack, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, p := range Catalog() {
		if p.Name == want {
			return p, true
		}
	}
	return Pack{}, false
}

// Names returns the catalog's pack names (for the session pointer).
func Names() []string {
	var out []string
	for _, p := range Catalog() {
		out = append(out, p.Name)
	}
	return out
}

// Full is the complete pack returned by the `knowledge` tool on consult — Facts AND Idioms,
// since a consult is the model deliberately reaching for the reference.
func (p Pack) Full() string {
	var b strings.Builder
	b.WriteString("# " + p.Name + " — knowledge pack\n\n")
	b.WriteString("## Facts (authoritative — these override your priors)\n")
	b.WriteString(strings.TrimSpace(p.Facts) + "\n")
	if id := strings.TrimSpace(p.Idioms); id != "" {
		b.WriteString("\n## Idioms (defaults for NEW code — match existing code, never refactor to satisfy these)\n")
		b.WriteString(id + "\n")
	}
	return b.String()
}

// parse reads a pack file: a `---` front-matter block (flat key: value, comma-separated lists)
// followed by `## Facts` and optional `## Idioms` sections. Hand-parsed in the same style as
// the skills package — no YAML dependency. A pack without a name or Facts is skipped.
func parse(body string) (Pack, bool) {
	if !strings.HasPrefix(body, "---") {
		return Pack{}, false
	}
	rest := strings.TrimPrefix(body, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return Pack{}, false
	}
	var p Pack
	for _, ln := range strings.Split(rest[:end], "\n") {
		k, v, found := strings.Cut(ln, ":")
		if !found {
			continue
		}
		val := strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "name":
			p.Name = strings.ToLower(val)
		case "triggers":
			p.Triggers = splitList(val)
		case "detect-files":
			p.Detect.Files = splitList(val)
		case "detect-deps":
			p.Detect.Deps = splitList(val)
		}
	}
	doc := rest[end+len("\n---"):]
	p.Facts = section(doc, "Facts")
	p.Idioms = section(doc, "Idioms")
	if p.Name == "" || strings.TrimSpace(p.Facts) == "" {
		return Pack{}, false
	}
	if len(p.Triggers) == 0 {
		p.Triggers = []string{p.Name} // default trigger is the pack's own name
	}
	return p, true
}

// section extracts the body of a `## <header>` block — everything until the next `## ` or EOF.
func section(doc, header string) string {
	lines := strings.Split(doc, "\n")
	var out []string
	in := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			in = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(ln, "## ")), header)
			continue
		}
		if in {
			out = append(out, ln)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// splitList parses a comma-separated front-matter value into lowercased, trimmed items.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if t := strings.ToLower(strings.TrimSpace(part)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// manifestDeps reads the dependency names from package.json (deps + devDeps) and go.mod's
// require lines — enough to fingerprint a stack. Best-effort: missing/unparseable → empty.
func manifestDeps(root string) map[string]bool {
	out := map[string]bool{}
	if b, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		var pkg struct {
			Dependencies    map[string]json.RawMessage `json:"dependencies"`
			DevDependencies map[string]json.RawMessage `json:"devDependencies"`
		}
		if json.Unmarshal(b, &pkg) == nil {
			for name := range pkg.Dependencies {
				out[strings.ToLower(name)] = true
			}
			for name := range pkg.DevDependencies {
				out[strings.ToLower(name)] = true
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			f := strings.Fields(strings.TrimSpace(ln))
			if len(f) >= 1 && strings.Contains(f[0], "/") {
				out[strings.ToLower(f[0])] = true
			}
		}
	}
	return out
}
