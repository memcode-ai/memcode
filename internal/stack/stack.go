// Package stack is a deterministic project tech-stack detector. It produces a
// StackFacts fact sheet from the repo on disk — language byte percentages plus
// frameworks/CLIs/databases/infra/CI parsed from manifests and config files — so
// `memcode stack` and /overview consume FACTS rather than inferring the stack from
// commit text or subsystem names (which hallucinates).
//
// Detection runs behind the StackDetector interface. LocalStackDetector is native,
// fast, dependency-free (the only path today). An APIStackDetector — heavier
// server-side classification — can slot in behind the same interface later.
package stack

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LanguageStat is one language's share of the codebase, by bytes.
type LanguageStat struct {
	Name  string  `json:"name"`
	Bytes int64   `json:"bytes"`
	Pct   float64 `json:"pct"`
}

// TechFact is a detected technology with the file(s) that prove it.
type TechFact struct {
	Name       string   `json:"name"`
	Category   string   `json:"category"`   // runtime|framework|cli|database|infra|ci
	Confidence string   `json:"confidence"` // high (manifest-declared) | medium (inferred)
	Evidence   []string `json:"evidence"`   // files the claim is grounded in
}

// RepoFacts describes the repository shape — VCS, whether it's a monorepo, and any
// workspace/build orchestrators — all from files on disk.
type RepoFacts struct {
	VCS       string   `json:"vcs,omitempty"`       // "git"
	Monorepo  bool     `json:"monorepo"`            // multiple modules or a workspace file
	Modules   int      `json:"modules"`             // count of manifest modules found
	Workspace []string `json:"workspace,omitempty"` // Turborepo, pnpm workspaces, Go workspace, …
}

// StackFacts is the deterministic fact sheet. Every claim is backed by Evidence.
type StackFacts struct {
	Repo       RepoFacts      `json:"repo"`
	Languages  []LanguageStat `json:"languages"`
	Runtimes   []TechFact     `json:"runtimes"`
	Frameworks []TechFact     `json:"frameworks"`
	CLIs       []TechFact     `json:"clis"`
	Databases  []TechFact     `json:"databases"`
	Infra      []TechFact     `json:"infra"`
	CI         []TechFact     `json:"ci"`
}

// StackDetector produces a StackFacts for a repo root.
type StackDetector interface {
	Detect(ctx context.Context, root string) (StackFacts, error)
}

// LocalStackDetector is the native, offline, dependency-free path.
type LocalStackDetector struct{}

var _ StackDetector = LocalStackDetector{}

func (LocalStackDetector) Detect(ctx context.Context, root string) (StackFacts, error) {
	f := StackFacts{Languages: scanLanguages(root)}
	add := func(t TechFact) {
		switch t.Category {
		case "runtime":
			f.Runtimes = appendFact(f.Runtimes, t)
		case "framework":
			f.Frameworks = appendFact(f.Frameworks, t)
		case "cli":
			f.CLIs = appendFact(f.CLIs, t)
		case "database":
			f.Databases = appendFact(f.Databases, t)
		case "infra":
			f.Infra = appendFact(f.Infra, t)
		case "ci":
			f.CI = appendFact(f.CI, t)
		}
	}
	detectGoMod(root, add)
	detectPackageJSON(root, add)
	detectConfigFiles(root, add)
	f.Repo = detectRepo(root)
	return f, nil
}

// detectRepo reads the repository shape from disk: VCS, module count (→ monorepo),
// and any workspace/build orchestrators.
func detectRepo(root string) RepoFacts {
	r := RepoFacts{}
	exists := func(rel string) bool { _, err := os.Stat(filepath.Join(root, rel)); return err == nil }
	if exists(".git") {
		r.VCS = "git"
	}
	r.Modules = len(findManifests(root, "go.mod")) + len(findManifests(root, "package.json")) +
		len(findManifests(root, "Cargo.toml")) + len(findManifests(root, "pyproject.toml"))
	for _, w := range []struct{ file, label string }{
		{"turbo.json", "Turborepo"},
		{"pnpm-workspace.yaml", "pnpm workspaces"},
		{"nx.json", "Nx"},
		{"lerna.json", "Lerna"},
		{"go.work", "Go workspace"},
	} {
		if exists(w.file) {
			r.Workspace = append(r.Workspace, w.label)
		}
	}
	r.Monorepo = r.Modules > 1 || len(r.Workspace) > 0
	return r
}

// appendFact merges by name (union the evidence) so one tech isn't listed twice.
func appendFact(into []TechFact, t TechFact) []TechFact {
	for i := range into {
		if into[i].Name == t.Name {
			into[i].Evidence = union(into[i].Evidence, t.Evidence)
			return into
		}
	}
	return append(into, t)
}

// --- language bytes (a mini-linguist) ---

var skipDirs = map[string]bool{
	".git": true, ".memcode": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, ".next": true, "out": true, "target": true,
	".turbo": true, "coverage": true, "testdata": true,
}

var extLang = map[string]string{
	".go": "Go", ".ts": "TypeScript", ".tsx": "TypeScript", ".js": "JavaScript",
	".jsx": "JavaScript", ".mjs": "JavaScript", ".py": "Python", ".rs": "Rust",
	".rb": "Ruby", ".java": "Java", ".kt": "Kotlin", ".c": "C", ".h": "C",
	".cpp": "C++", ".cc": "C++", ".cs": "C#", ".sh": "Shell", ".bash": "Shell",
	".md": "Markdown", ".yaml": "YAML", ".yml": "YAML", ".json": "JSON",
	".toml": "TOML", ".html": "HTML", ".css": "CSS", ".scss": "CSS", ".sql": "SQL",
	".proto": "Protobuf", ".tf": "HCL",
}

func scanLanguages(root string) []LanguageStat {
	bytes := map[string]int64{}
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && d.Name() != "." && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		lang, ok := extLang[strings.ToLower(filepath.Ext(path))]
		if !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		bytes[lang] += info.Size()
		total += info.Size()
		return nil
	})
	out := make([]LanguageStat, 0, len(bytes))
	for name, b := range bytes {
		pct := 0.0
		if total > 0 {
			pct = float64(b) / float64(total) * 100
		}
		out = append(out, LanguageStat{Name: name, Bytes: b, Pct: pct})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

// --- manifest + config detection ---

type depRule struct{ match, name, category string }

// goDeps maps a go.mod module path substring → a friendly tech fact.
var goDeps = []depRule{
	{"bubbletea", "Bubble Tea (TUI)", "framework"},
	{"charmbracelet/lipgloss", "Lip Gloss", "framework"},
	{"charmbracelet/bubbles", "Bubbles", "framework"},
	{"alecthomas/chroma", "Chroma (syntax highlighting)", "framework"},
	{"spf13/cobra", "Cobra", "cli"},
	{"urfave/cli", "urfave/cli", "cli"},
	{"mattn/go-sqlite3", "SQLite", "database"},
	{"modernc.org/sqlite", "SQLite", "database"},
	{"cloud.google.com/go/firestore", "Firestore", "database"},
	{"redis/go-redis", "Redis", "database"},
	{"jackc/pgx", "PostgreSQL", "database"},
	{"cloud.google.com/go", "Google Cloud", "infra"},
	{"aws/aws-sdk-go", "AWS", "infra"},
}

// nodeDeps maps a package.json dependency name → a friendly tech fact.
var nodeDeps = []depRule{
	{"next", "Next.js", "framework"},
	{"react", "React", "framework"},
	{"vue", "Vue", "framework"},
	{"svelte", "Svelte", "framework"},
	{"tailwindcss", "Tailwind CSS", "framework"},
	{"typescript", "TypeScript", "runtime"},
	{"vite", "Vite", "framework"},
	{"express", "Express", "framework"},
}

// findManifests returns every file named `base` under root (skipping vendored/
// ignored dirs), so a monorepo with go.mod in cli/ and api/ — not root — is fully
// detected. Paths are relative to root for clean evidence.
func findManifests(root, base string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == base {
			if rel, err := filepath.Rel(root, path); err == nil {
				out = append(out, rel)
			}
		}
		return nil
	})
	return out
}

func detectGoMod(root string, add func(TechFact)) {
	for _, rel := range findManifests(root, "go.mod") {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		src := string(b)
		for _, ln := range strings.Split(src, "\n") {
			ln = strings.TrimSpace(ln)
			if v, ok := strings.CutPrefix(ln, "go "); ok && len(v) > 0 && v[0] >= '0' && v[0] <= '9' {
				add(TechFact{Name: "Go " + v, Category: "runtime", Confidence: "high", Evidence: []string{rel}})
			}
		}
		for _, r := range goDeps {
			if strings.Contains(src, r.match) {
				add(TechFact{Name: r.name, Category: r.category, Confidence: "high", Evidence: []string{rel}})
			}
		}
	}
}

func detectPackageJSON(root string, add func(TechFact)) {
	for _, rel := range findManifests(root, "package.json") {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		var pkg struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if json.Unmarshal(b, &pkg) != nil {
			continue
		}
		add(TechFact{Name: "Node.js", Category: "runtime", Confidence: "high", Evidence: []string{rel}})
		has := func(name string) bool {
			_, a := pkg.Dependencies[name]
			_, d := pkg.DevDependencies[name]
			return a || d
		}
		for _, r := range nodeDeps {
			if has(r.match) {
				add(TechFact{Name: r.name, Category: r.category, Confidence: "high", Evidence: []string{rel}})
			}
		}
	}
}

func detectConfigFiles(root string, add func(TechFact)) {
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, rel))
		return err == nil
	}
	if exists("Dockerfile") || exists("api/Dockerfile") || exists("cli/Dockerfile") {
		add(TechFact{Name: "Docker", Category: "infra", Confidence: "high", Evidence: []string{"Dockerfile"}})
	}
	for _, c := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yaml"} {
		if exists(c) {
			add(TechFact{Name: "Docker Compose", Category: "infra", Confidence: "high", Evidence: []string{c}})
		}
	}
	if exists("pyproject.toml") || exists("requirements.txt") {
		add(TechFact{Name: "Python", Category: "runtime", Confidence: "high", Evidence: []string{"pyproject.toml"}})
	}
	if exists("Cargo.toml") {
		add(TechFact{Name: "Rust (Cargo)", Category: "runtime", Confidence: "high", Evidence: []string{"Cargo.toml"}})
	}
	if exists(".github/workflows") {
		add(TechFact{Name: "GitHub Actions", Category: "ci", Confidence: "high", Evidence: []string{".github/workflows"}})
	}
	if exists("Makefile") {
		add(TechFact{Name: "Make", Category: "infra", Confidence: "high", Evidence: []string{"Makefile"}})
	}
	if exists("infra/gcp") || exists("cloudbuild.yaml") {
		add(TechFact{Name: "Google Cloud", Category: "infra", Confidence: "high", Evidence: []string{"infra/"}})
	}
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
