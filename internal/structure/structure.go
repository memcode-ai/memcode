// Package structure derives a repository's topology — its subsystems and the
// dependencies between them — from language-agnostic, deterministic signals:
// the directory tree, dependency manifests, ownership and change history. It
// deliberately does NOT parse code. Symbol-level detail is a later, per-language
// drill-down; the top-down model is what humans actually navigate.
package structure

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/memcode-ai/memcode/internal/store"
)

// Subsystem is a bounded implementation unit (a Go module, an npm package, a
// Cargo crate, …) — the "where" of the system.
type Subsystem struct {
	Key       string   `json:"key"`       // path relative to repo root
	Name      string   `json:"name"`      // human label (package name or dir name)
	Ecosystem string   `json:"ecosystem"` // node | go | rust | python | java
	Package   string   `json:"package,omitempty"`
	Manifest  string   `json:"manifest"`       // manifest filename
	Docs      []string `json:"docs,omitempty"` // doc files in this subsystem
	Commits   int      `json:"commits"`        // all-time commits touching this subsystem
	Recent    int      `json:"recent_commits"` // commits in the recent window
	// Churn-weighted hotspot signals over the recent window. Commit COUNT alone is
	// a weak "where's the work" proxy — many tiny commits in one area drown out
	// fewer, deeper changes elsewhere. Lines changed + active days capture depth
	// and sustained effort; Hotness blends them (normalized across subsystems).
	RecentChurn int      `json:"recent_churn,omitempty"` // added+deleted lines in the recent window
	RecentDays  int      `json:"recent_days,omitempty"`  // distinct days with a commit in the window
	Hotness     float64  `json:"hotness,omitempty"`      // blended recent-activity score (0..1)
	Owners      []string `json:"owners,omitempty"`
}

// Dependency is a directed edge between two subsystems (by key).
type Dependency struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Result is the topology produced by a scan.
type Result struct {
	Root        string       `json:"root"`
	GeneratedAt time.Time    `json:"generated_at"`
	Subsystems  []Subsystem  `json:"subsystems"`
	Deps        []Dependency `json:"dependencies"`
	Docs        []string     `json:"docs,omitempty"` // repo-level docs (README, CLAUDE.md, …)
}

// Scan builds the topology for the repo at root, persists it (subsystem
// entities, depends_on edges, and the structural current_state for scope "repo")
// and returns it for display.
func Scan(ctx context.Context, s store.Store, root string) (Result, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Result{}, err
	}

	manifests, err := discoverManifests(ctx, root)
	if err != nil {
		return Result{}, err
	}

	res := Result{
		Root:        root,
		GeneratedAt: time.Now().UTC(),
		Docs:        repoDocs(root),
	}

	git := newGit(root) // nil-safe: degrades gracefully when not a git repo

	// Decide which manifests are real subsystems vs. workspace aggregators.
	for _, m := range selectSubsystems(manifests) {
		sub := Subsystem{
			Key:       m.rel,
			Name:      subsystemName(m),
			Ecosystem: m.ecosystem,
			Package:   m.pkgName,
			Manifest:  m.file,
			Docs:      subsystemDocs(m.dir),
		}
		sub.Commits, sub.Recent, sub.Owners = git.stats(ctx, m.rel)
		sub.RecentChurn, sub.RecentDays = git.recentActivity(ctx, m.rel)
		res.Subsystems = append(res.Subsystems, sub)
	}
	assignHotness(res.Subsystems)
	sort.Slice(res.Subsystems, func(i, j int) bool {
		return res.Subsystems[i].Key < res.Subsystems[j].Key
	})

	res.Deps = resolveDeps(manifests, res.Subsystems)

	if err := persist(ctx, s, res); err != nil {
		return Result{}, err
	}
	return res, nil
}

// assignHotness blends the recent-activity signals into a single 0..1 score per
// subsystem. Each dimension is max-normalized across the set so no single raw
// magnitude dominates (churn in the thousands vs. a handful of commits), then
// weighted: churn carries the most signal about depth of work, recent commits
// next, active days last (sustained vs. one-off). All-zero (no recent activity)
// leaves Hotness 0, so ranking falls back to all-time commits.
func assignHotness(subs []Subsystem) {
	var maxChurn, maxRecent, maxDays float64
	for _, s := range subs {
		maxChurn = maxF(maxChurn, float64(s.RecentChurn))
		maxRecent = maxF(maxRecent, float64(s.Recent))
		maxDays = maxF(maxDays, float64(s.RecentDays))
	}
	norm := func(v, max float64) float64 {
		if max <= 0 {
			return 0
		}
		return v / max
	}
	for i := range subs {
		subs[i].Hotness = 0.5*norm(float64(subs[i].RecentChurn), maxChurn) +
			0.3*norm(float64(subs[i].Recent), maxRecent) +
			0.2*norm(float64(subs[i].RecentDays), maxDays)
	}
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// ByHotness returns a copy of subs ranked by recent-activity hotness (churn +
// commits + active days, last 30d), with all-time commits and key as stable
// tiebreakers. This is the canonical "where is the work now" ordering.
func ByHotness(subs []Subsystem) []Subsystem {
	out := append([]Subsystem(nil), subs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hotness != out[j].Hotness {
			return out[i].Hotness > out[j].Hotness
		}
		if out[i].Recent != out[j].Recent {
			return out[i].Recent > out[j].Recent
		}
		if out[i].Commits != out[j].Commits {
			return out[i].Commits > out[j].Commits
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// selectSubsystems picks leaf subsystems: every non-root manifest, or — for a
// single-package repo — the root manifest itself. Workspace aggregators are
// dropped so we don't model the monorepo root as a subsystem.
func selectSubsystems(manifests []manifest) []manifest {
	var children, roots []manifest
	for _, m := range manifests {
		if m.rel == "." {
			roots = append(roots, m)
		} else {
			children = append(children, m)
		}
	}
	if len(children) > 0 {
		return children
	}
	// No children: keep the root manifest(s) that aren't pure aggregators.
	var out []manifest
	for _, m := range roots {
		if !m.isWSRoot {
			out = append(out, m)
		}
	}
	return out
}

// resolveDeps links subsystems via their declared dependencies (intra-repo only).
func resolveDeps(manifests []manifest, subs []Subsystem) []Dependency {
	// package name -> subsystem key
	byPkg := map[string]string{}
	bySub := map[string]Subsystem{}
	for _, sub := range subs {
		bySub[sub.Key] = sub
		if sub.Package != "" {
			byPkg[sub.Package] = sub.Key
		}
	}
	// manifest by rel, to read each subsystem's deps
	mByRel := map[string]manifest{}
	for _, m := range manifests {
		mByRel[m.rel] = m
	}

	seen := map[Dependency]bool{}
	var out []Dependency
	for _, sub := range subs {
		m, ok := mByRel[sub.Key]
		if !ok {
			continue
		}
		for _, dep := range m.deps {
			target, ok := byPkg[dep]
			if !ok || target == sub.Key {
				continue
			}
			d := Dependency{From: sub.Key, To: target}
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// persist writes subsystem entities, depends_on edges and the structural state.
func persist(ctx context.Context, s store.Store, res Result) error {
	for _, sub := range res.Subsystems {
		attrs, _ := json.Marshal(sub)
		if err := s.UpsertEntity(ctx, store.Entity{
			Kind:  "subsystem",
			Key:   sub.Key,
			Attrs: attrs,
		}); err != nil {
			return err
		}
	}
	for _, d := range res.Deps {
		if err := s.UpsertEdge(ctx, store.Edge{
			Src:  "subsystem:" + d.From,
			Dst:  "subsystem:" + d.To,
			Kind: "depends_on",
		}); err != nil {
			return err
		}
	}
	body, _ := json.Marshal(res)
	return s.PutState(ctx, store.State{
		Scope:       "repo",
		Layer:       "structural",
		Body:        body,
		RefreshedAt: res.GeneratedAt,
	})
}

func subsystemName(m manifest) string {
	if m.pkgName != "" {
		return m.pkgName
	}
	return filepath.Base(m.dir)
}

// repoDocs finds top-level documents that declare intent (higher-trust signals
// than directory names).
func repoDocs(root string) []string {
	candidates := []string{
		"README.md", "README", "CLAUDE.md", "AGENTS.md",
		".cursorrules", "CONTRIBUTING.md", "ARCHITECTURE.md",
	}
	var out []string
	for _, c := range candidates {
		if fileExists(filepath.Join(root, c)) {
			out = append(out, c)
		}
	}
	for _, dir := range []string{"docs", filepath.Join(".cursor", "rules"), filepath.Join("docs", "adr")} {
		if info, err := os.Stat(filepath.Join(root, dir)); err == nil && info.IsDir() {
			out = append(out, dir+"/")
		}
	}
	return out
}

func subsystemDocs(dir string) []string {
	var out []string
	for _, c := range []string{"README.md", "README"} {
		if fileExists(filepath.Join(dir, c)) {
			out = append(out, c)
		}
	}
	return out
}
