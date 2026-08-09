package structure

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/memcode-ai/memcode/internal/repofiles"
)

// Ecosystem labels the package manager / language family a subsystem belongs to.
// These abstractions are language-agnostic on purpose — a subsystem is a subsystem
// whether it's Go, Node, Rust, Python or Java.
const (
	EcoNode   = "node"
	EcoGo     = "go"
	EcoRust   = "rust"
	EcoPython = "python"
	EcoJava   = "java"
)

// manifest is a discovered package/module descriptor on disk.
type manifest struct {
	dir       string // absolute directory containing the manifest
	rel       string // path relative to the repo root ("." for root)
	file      string // manifest filename
	ecosystem string
	pkgName   string   // declared package name (for intra-repo dependency resolution)
	deps      []string // dependency names (used only to link subsystems within the repo)
	isWSRoot  bool     // declares a workspace (aggregator, not a leaf subsystem)
}

// discoverManifests returns every package/module manifest among the project's
// non-ignored files (so a gitignored or vendored manifest never becomes a
// subsystem).
func discoverManifests(ctx context.Context, root string) ([]manifest, error) {
	var out []manifest
	for _, rel := range repofiles.List(ctx, root) {
		if m, ok := parseManifest(root, filepath.Join(root, rel), filepath.Base(rel)); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// parseManifest recognizes a manifest file and extracts the bits we need.
func parseManifest(root, path, name string) (manifest, bool) {
	dir := filepath.Dir(path)
	rel, _ := filepath.Rel(root, dir)
	if rel == "" {
		rel = "."
	}
	base := manifest{dir: dir, rel: rel, file: name}

	switch name {
	case "package.json":
		return parsePackageJSON(base, path)
	case "go.mod":
		base.ecosystem = EcoGo
		base.pkgName = goModulePath(path)
		base.isWSRoot = fileExists(filepath.Join(dir, "go.work"))
		return base, true
	case "Cargo.toml":
		return parseCargoToml(base, path)
	case "pyproject.toml":
		return parsePyproject(base, path)
	case "pom.xml":
		return parsePomXML(base, path)
	}
	return manifest{}, false
}

func parsePackageJSON(base manifest, path string) (manifest, bool) {
	var pj struct {
		Name            string            `json:"name"`
		Workspaces      json.RawMessage   `json:"workspaces"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := readJSON(path, &pj); err != nil {
		return manifest{}, false
	}
	base.ecosystem = EcoNode
	base.pkgName = pj.Name
	// A package.json with a "workspaces" field, or a sibling pnpm-workspace.yaml,
	// is a monorepo aggregator rather than a leaf subsystem.
	base.isWSRoot = len(pj.Workspaces) > 0 || fileExists(filepath.Join(base.dir, "pnpm-workspace.yaml"))
	for name := range pj.Dependencies {
		base.deps = append(base.deps, name)
	}
	for name := range pj.DevDependencies {
		base.deps = append(base.deps, name)
	}
	return base, true
}

func parseCargoToml(base manifest, path string) (manifest, bool) {
	var ct struct {
		Package      struct{ Name string } `toml:"package"`
		Workspace    map[string]any        `toml:"workspace"`
		Dependencies map[string]any        `toml:"dependencies"`
	}
	if _, err := toml.DecodeFile(path, &ct); err != nil {
		return manifest{}, false
	}
	base.ecosystem = EcoRust
	base.pkgName = ct.Package.Name
	base.isWSRoot = len(ct.Workspace) > 0
	for name := range ct.Dependencies {
		base.deps = append(base.deps, name)
	}
	return base, true
}

func parsePyproject(base manifest, path string) (manifest, bool) {
	var pp struct {
		Project struct {
			Name         string   `toml:"name"`
			Dependencies []string `toml:"dependencies"`
		} `toml:"project"`
		Tool struct {
			Poetry struct {
				Name string `toml:"name"`
			} `toml:"poetry"`
		} `toml:"tool"`
	}
	if _, err := toml.DecodeFile(path, &pp); err != nil {
		return manifest{}, false
	}
	base.ecosystem = EcoPython
	base.pkgName = pp.Project.Name
	if base.pkgName == "" {
		base.pkgName = pp.Tool.Poetry.Name
	}
	for _, spec := range pp.Project.Dependencies {
		base.deps = append(base.deps, pep508Name(spec))
	}
	return base, true
}

// --- small parse helpers ---

var (
	reGoModule = regexp.MustCompile(`(?m)^module\s+(\S+)`)
	rePEP508   = regexp.MustCompile(`^[A-Za-z0-9._-]+`)
)

// pomProject reads a Maven pom.xml's OWN artifactId with a real XML parser.
// encoding/xml binds <artifactId> only as a DIRECT child of <project>, so a
// <parent>/<dependency> artifactId (nested) is correctly ignored — unlike a regex,
// which grabbed the first match (often the parent's). NEVER parse markup with regex.
type pomProject struct {
	XMLName    xml.Name `xml:"project"`
	ArtifactID string   `xml:"artifactId"`
}

func parsePomXML(base manifest, path string) (manifest, bool) {
	base.ecosystem = EcoJava
	if b, err := os.ReadFile(path); err == nil {
		var p pomProject
		if xml.Unmarshal(b, &p) == nil {
			base.pkgName = strings.TrimSpace(p.ArtifactID)
		}
	}
	return base, true
}

func goModulePath(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if m := reGoModule.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// pep508Name extracts the distribution name from a PEP 508 dependency spec,
// e.g. "requests>=2.0" -> "requests".
func pep508Name(spec string) string {
	return rePEP508.FindString(strings.TrimSpace(spec))
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
