// Package gitlog is a tiny, dependency-free reader over `git log`, shared by the
// context compiler, provenance, predict and producer attribution. All functions
// degrade to empty results when git is unavailable or the path has no history.
package gitlog

import (
	"context"
	"os/exec"
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
// repo), newest first.
func Recent(ctx context.Context, root, path string, n int) []Commit {
	return logRecords(ctx, root, append([]string{"-n", strconv.Itoa(n)}, pathspec(path)...)...)
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
