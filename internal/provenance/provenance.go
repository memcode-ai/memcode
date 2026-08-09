// Package provenance answers "why is this here?" for a file, directory or
// subsystem — the question humans constantly ask and agents are bad at. It is a
// deterministic query over what the engine already stores (git history,
// topology, objectives, doctrine) plus a test-file heuristic. No model.
package provenance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/gitlog"
	"github.com/memcode-ai/memcode/internal/objectives"
	"github.com/memcode-ai/memcode/internal/sources"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
)

// Goal is an objective this target serves.
type Goal struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// Provenance is the structured "why" for a target.
type Provenance struct {
	Target        string          `json:"target"`
	Subsystem     string          `json:"subsystem,omitempty"`
	Introduced    *gitlog.Commit  `json:"introduced,omitempty"`
	Evolved       []gitlog.Commit `json:"evolved"`
	Serves        []Goal          `json:"serves"`
	ConstrainedBy []string        `json:"constrained_by"`
	TestedBy      []string        `json:"tested_by"`
	DependsOn     []string        `json:"depends_on"`
	DependedBy    []string        `json:"depended_by"`
	Notes         []string        `json:"notes"`
}

// Why builds the provenance for a target, which may be a path or a subsystem
// name/key.
func Why(ctx context.Context, st store.Store, root, target string) (Provenance, error) {
	topo, err := structure.Load(ctx, st)
	if err != nil {
		return Provenance{}, err
	}

	p := Provenance{
		Target:        target,
		Evolved:       []gitlog.Commit{},
		Serves:        []Goal{},
		ConstrainedBy: []string{},
		TestedBy:      []string{},
		DependsOn:     []string{},
		DependedBy:    []string{},
		Notes:         []string{},
	}

	rel, isPath := structure.ResolvePath(root, target)
	var sub structure.Subsystem
	var haveSub bool

	if isPath {
		sub, haveSub = structure.ContainingSubsystem(topo.Subsystems, rel)
	} else if s, ok := matchSubsystem(topo.Subsystems, target); ok {
		sub, haveSub = s, true
		rel = sub.Key
	} else {
		return Provenance{}, &notFoundError{target}
	}
	if haveSub {
		p.Subsystem = sub.Key
	}

	gitPath := rel
	if gitPath == "" {
		gitPath = "."
	}

	if c, ok := gitlog.First(ctx, root, gitPath); ok {
		p.Introduced = &c
	} else {
		p.Notes = append(p.Notes, "no git history for this path yet (uncommitted?)")
	}
	p.Evolved = gitlog.Recent(ctx, root, gitPath, 6)

	// Serves: objectives in flight. v1 tracks these at the repo level — per-target
	// linkage arrives with `learn`.
	if cur, err := objectives.New(st).Current(ctx); err == nil {
		for _, o := range cur {
			p.Serves = append(p.Serves, Goal{ID: o.ID, Title: o.Title, Status: o.Status})
		}
		if len(cur) > 0 {
			p.Notes = append(p.Notes, "objectives are repo-level in v1 (per-target linkage lands with `learn`)")
		}
	}

	// Constrained by: the instruction/doc sources that GOVERN this target's scope.
	if srcs, err := sources.Load(ctx, st); err == nil {
		for _, s := range srcs {
			if !s.AppliesTo(rel) {
				continue
			}
			tag := s.Path + " — " + s.Kind + " (" + s.Scope + ")"
			if s.Stale {
				tag += " [STALE?]"
			}
			p.ConstrainedBy = append(p.ConstrainedBy, tag)
		}
	}
	if len(p.ConstrainedBy) == 0 {
		p.Notes = append(p.Notes, "no instruction/doc sources govern this area")
	}

	p.TestedBy = findTests(root, rel, isPath)

	if haveSub {
		for _, d := range topo.Deps {
			switch {
			case d.From == sub.Key:
				p.DependsOn = append(p.DependsOn, d.To)
			case d.To == sub.Key:
				p.DependedBy = append(p.DependedBy, d.From)
			}
		}
	}
	return p, nil
}

type notFoundError struct{ target string }

func (e *notFoundError) Error() string {
	return fmt.Sprintf("no path or subsystem matching %q", e.target)
}

// --- resolution helpers ---

func matchSubsystem(subs []structure.Subsystem, q string) (structure.Subsystem, bool) {
	for _, s := range subs {
		if s.Key == q || s.Package == q {
			return s, true
		}
	}
	for _, s := range subs {
		if filepath.Base(s.Key) == q || strings.HasSuffix(s.Key, "/"+q) {
			return s, true
		}
	}
	return structure.Subsystem{}, false
}

// findTests locates likely test files for the target.
func findTests(root, rel string, isPath bool) []string {
	if rel == "" {
		return nil
	}
	abs := filepath.Join(root, rel)
	info, err := os.Stat(abs)
	if err != nil {
		return nil
	}

	var out []string
	add := func(r string) {
		if r != "" && fileExists(filepath.Join(root, r)) && !contains(out, r) {
			out = append(out, r)
		}
	}

	if info.IsDir() {
		_ = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
			if err != nil || len(out) >= 12 {
				return err
			}
			if d.IsDir() {
				if path != abs && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if isTestName(d.Name()) {
				if r, e := filepath.Rel(root, path); e == nil {
					add(filepath.ToSlash(r))
				}
			}
			return nil
		})
		return out
	}

	dir := filepath.Dir(rel)
	ext := filepath.Ext(rel)
	base := strings.TrimSuffix(filepath.Base(rel), ext)
	add(filepath.ToSlash(filepath.Join(dir, base+"_test.go")))
	add(filepath.ToSlash(filepath.Join(dir, base+".test"+ext)))
	add(filepath.ToSlash(filepath.Join(dir, base+".spec"+ext)))
	add(filepath.ToSlash(filepath.Join(dir, "__tests__", base+".test"+ext)))
	return out
}

func isTestName(name string) bool {
	return strings.Contains(name, "_test.") || strings.Contains(name, ".test.") ||
		strings.Contains(name, ".spec.")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
