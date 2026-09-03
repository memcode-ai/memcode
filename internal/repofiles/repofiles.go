// Package repofiles enumerates the files that are actually part of a project —
// honoring .gitignore — so the engine never mistakes vendored, generated,
// cached or ignored files (node_modules, docker volumes, build output) for real
// source. It uses `git ls-files` (tracked + untracked-but-not-ignored) and falls
// back to a filtered filesystem walk only when the directory isn't a git repo.
package repofiles

import (
	"context"
	"io/fs"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// List returns project files as slash paths relative to root, excluding anything git ignores
// AND anything inside a VENDORED tree (vendor/, internal/forks/, third_party/, …) — code that
// is tracked but is a checked-in third-party dependency, not this project's own source. (A
// first-party submodule like services/api is NOT vendored and stays — structure detection wants
// it.) Order is unspecified.
func List(ctx context.Context, root string) []string {
	var files []string
	if g, ok := gitList(ctx, root); ok {
		files = g
	} else {
		files = walkList(ctx, root)
	}
	return dropVendored(files)
}

// vendoredSegment names a path segment that marks a checked-in third-party tree. These are
// dependencies, not project source, so exploration/recall skips them (e.g. memcode's vendored
// vaxis fork under internal/forks/).
var vendoredSegment = map[string]bool{
	"vendor": true, "third_party": true, "third-party": true, "forks": true,
}

func dropVendored(files []string) []string {
	out := files[:0]
	for _, f := range files {
		vendored := false
		for _, seg := range strings.Split(path.Dir(f), "/") {
			if vendoredSegment[seg] {
				vendored = true
				break
			}
		}
		if !vendored {
			out = append(out, f)
		}
	}
	return out
}

func gitList(ctx context.Context, root string) ([]string, bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false
	}
	// tracked + untracked-not-ignored = what git considers part of the project.
	cmd := exec.CommandContext(ctx, "git", "-C", root,
		"ls-files", "--cached", "--others", "--exclude-standard", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			files = append(files, filepath.ToSlash(p))
		}
	}
	return files, true
}

// fallbackSkip mirrors common ignore conventions for non-git directories.
var fallbackSkip = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true, ".next": true,
	"out": true, "vendor": true, ".memcode": true, ".turbo": true, "target": true,
	"__pycache__": true, "venv": true, "site-packages": true,
}

// The non-git fallback walk is BOUNDED. Outside a repo there is no gitignore
// to trust and no reason to believe the tree is a project at all — a first
// run in $HOME would otherwise crawl Dropbox, media, and every other repo on
// the machine for minutes before the prompt ever appears. Real non-git
// projects fit comfortably under these caps; anything that trips them is a
// directory we shouldn't be indexing wholesale anyway.
const (
	walkMaxFiles = 20000
	walkMaxDepth = 12
)

func walkList(ctx context.Context, root string) []string {
	var files []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return nil
		}
		depth := strings.Count(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			if path != root && (fallbackSkip[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			if depth >= walkMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= walkMaxFiles {
			return filepath.SkipAll
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files
}
