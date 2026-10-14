// Package gitlog is a tiny, dependency-free reader over `git log`, shared by the
// context compiler, provenance, predict and producer attribution. All functions
// degrade to empty results when git is unavailable or the path has no history.
package gitlog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Commit is one entry from git history.
type Commit struct {
	Hash        string `json:"hash"`
	Author      string `json:"author"`
	AuthorEmail string `json:"author_email"`
	Date        string `json:"date"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
}

// Fields are unit-separated; commits are NUL-separated (-z) so multi-line bodies
// don't break parsing.
const format = "%h\x1f%an\x1f%ae\x1f%ad\x1f%s\x1f%b"

// Recent returns up to n most-recent commits touching path ("" or "." = whole
// repo), newest first. For the whole-repo case, root may be the parent of
// several independent repositories (e.g. two sibling projects under one
// non-git workspace dir) rather than a repo itself — Roots discovers every
// repo underneath and the results are merged by date.
func Recent(ctx context.Context, root, path string, n int) []Commit {
	if path != "" && path != "." {
		return logRecords(ctx, root, append([]string{"-n", strconv.Itoa(n)}, pathspec(path)...)...)
	}
	roots := Roots(ctx, root)
	if len(roots) <= 1 {
		// Common case, byte-for-byte unchanged: root is itself a single repo
		// (or no repo at all), so query it directly exactly as before.
		return logRecords(ctx, root, "-n", strconv.Itoa(n))
	}
	var all []Commit
	for _, r := range roots {
		all = append(all, logRecords(ctx, r, "-n", strconv.Itoa(n))...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Date > all[j].Date })
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// rootsMaxDepth and rootsMaxVisited bound the recursive repo discovery below
// so a pathological tree (deep, no matching excludes) degrades gracefully
// instead of walking indefinitely.
const (
	rootsMaxDepth   = 6
	rootsMaxVisited = 5000
)

// rootsSkipDir names directories that are never worth descending into while
// looking for nested git repositories: dependency/build output that can
// legitimately contain its own vendored ".git"-less trees, or (for vendored
// submodules/subtrees) wouldn't be a root worth treating as independent
// history anyway.
var rootsSkipDir = map[string]bool{
	"node_modules": true, "vendor": true, "dist": true, "build": true,
	".next": true, "target": true, "__pycache__": true, ".turbo": true,
	"out": true, "venv": true, "site-packages": true,
}

// Roots discovers every independent git work-tree root at or under dir. If
// dir itself is inside a work tree, that single root is returned — the
// common single-repo case, unchanged in cost and result. Otherwise dir is
// walked (bounded by depth and total directories visited) looking for nested
// ".git" entries; each one found is recorded as a root and its subtree is
// NOT descended into further, since it owns everything below it. Hidden
// directories and common dependency/build directories are skipped. Order is
// deterministic (sorted).
func Roots(ctx context.Context, dir string) []string {
	if top, ok := workTreeRoot(ctx, dir); ok {
		return []string{top}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	var roots []string
	visited := 0
	var walk func(path string, depth int)
	walk = func(path string, depth int) {
		if ctx.Err() != nil || visited >= rootsMaxVisited || depth > rootsMaxDepth {
			return
		}
		visited++
		if info, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			_ = info
			roots = append(roots, path)
			return // this subtree owns its own history; don't descend further
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") || rootsSkipDir[name] {
				continue
			}
			walk(filepath.Join(path, name), depth+1)
		}
	}
	walk(abs, 0)
	sort.Strings(roots)
	return roots
}

// workTreeRoot reports the top-level directory of the git work tree
// containing dir, if any.
func workTreeRoot(ctx context.Context, dir string) (string, bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", false
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", false
	}
	return top, true
}

// First returns the commit that introduced path, if any.
func First(ctx context.Context, root, path string) (Commit, bool) {
	cs := logRecords(ctx, root, append([]string{"--reverse"}, pathspec(path)...)...)
	if len(cs) == 0 {
		return Commit{}, false
	}
	return cs[0], true
}

func pathspec(path string) []string {
	if path != "" && path != "." {
		return []string{"--", path}
	}
	return nil
}

func logRecords(ctx context.Context, root string, args ...string) []Commit {
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	full := append([]string{"-C", root, "log", "-z", "--date=short", "--format=" + format}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return nil
	}
	var commits []Commit
	for _, rec := range strings.Split(string(out), "\x00") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 6)
		if len(f) < 5 {
			continue
		}
		c := Commit{Hash: f[0], Author: f[1], AuthorEmail: f[2], Date: f[3], Subject: f[4]}
		if len(f) == 6 {
			c.Body = strings.TrimSpace(f[5])
		}
		commits = append(commits, c)
	}
	return commits
}
