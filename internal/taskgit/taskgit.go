// Package taskgit owns the git plumbing an autonomous run needs, deterministically.
//
// The division of labour is the point: the model decides WHAT should change;
// this package decides where that happens and how it is recorded. No model
// output reaches a git command here. Unattended `git` driven by generated text
// is where autonomous coding goes wrong, and the failure is silent — a wrong
// branch, a lost commit, a dirty checkout someone else was using.
//
// Milestone 4 covers isolation and revisions. Branch, commit, push and PR are
// milestone 5 and deliberately absent.
package taskgit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/statedir"
)

// Worktree is an isolated checkout a run executes in.
type Worktree struct {
	// Path is the checkout the run works in. Never the user's own.
	Path string
	// Branch is the ref created for this run.
	Branch string
	// Base is the commit the worktree was created from, resolved at EXECUTION
	// time. A Monday occurrence recovered on Wednesday builds on Wednesday's
	// HEAD, and recording it is what keeps that honest rather than implying the
	// repository was frozen at the occurrence.
	Base string
	// Repo is the repository the worktree belongs to.
	Repo string
}

// git runs a git command in dir and returns trimmed stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// IsRepo reports whether dir is inside a git working tree.
func IsRepo(ctx context.Context, dir string) bool {
	out, err := git(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// HeadRevision resolves the repository's current commit.
func HeadRevision(ctx context.Context, dir string) (string, error) {
	return git(ctx, dir, "rev-parse", "HEAD")
}

// WorktreeRoot is where run worktrees live: inside the repo's own .memcode, so
// they are ignored by git, obviously disposable, and easy to find when a failed
// run needs inspecting.
func WorktreeRoot(repo string) string {
	return filepath.Join(statedir.For(repo), "worktrees")
}

// BranchName renders a run's branch from the task's pattern.
// {name} and {date} are substituted; {run} disambiguates two runs of one task on
// one day, so a retry never collides with the run it is retrying.
func BranchName(pattern, taskName, runID string, now time.Time) string {
	b := pattern
	if strings.TrimSpace(b) == "" {
		b = "auto/{name}-{date}"
	}
	short := runID
	if i := strings.LastIndexByte(short, '_'); i >= 0 {
		short = short[i+1:]
	}
	if len(short) > 8 {
		short = short[:8]
	}
	b = strings.ReplaceAll(b, "{name}", taskName)
	b = strings.ReplaceAll(b, "{date}", now.UTC().Format("20060102"))
	b = strings.ReplaceAll(b, "{run}", short)
	if !strings.Contains(pattern, "{run}") {
		b += "-" + short
	}
	return sanitizeRef(b)
}

// sanitizeRef keeps a branch name to characters git accepts, so a task name or
// pattern can never produce an unusable or surprising ref.
func sanitizeRef(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '/', r == '.', r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-./")
	if out == "" {
		out = "auto-run"
	}
	return out
}

// Create makes a fresh worktree on a new branch off current HEAD.
//
// A mutating run NEVER executes in the user's active checkout. Isolation is not
// politeness: an unattended process editing the tree someone is working in can
// destroy uncommitted work, and a failed run would leave that tree wrecked with
// no obvious cause.
func Create(ctx context.Context, repo, branch string) (Worktree, error) {
	if !IsRepo(ctx, repo) {
		return Worktree{}, fmt.Errorf("%s is not a git repository — a task that changes code needs one", repo)
	}
	base, err := HeadRevision(ctx, repo)
	if err != nil {
		return Worktree{}, fmt.Errorf("resolving HEAD: %w", err)
	}
	root := WorktreeRoot(repo)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Worktree{}, err
	}
	path := filepath.Join(root, branchDir(branch))
	if _, err := os.Stat(path); err == nil {
		return Worktree{}, fmt.Errorf("worktree %s already exists", path)
	}
	if _, err := git(ctx, repo, "worktree", "add", "-b", branch, path, base); err != nil {
		return Worktree{}, err
	}
	return Worktree{Path: path, Branch: branch, Base: base, Repo: repo}, nil
}

// branchDir flattens a branch name into one directory component.
func branchDir(branch string) string { return strings.ReplaceAll(branch, "/", "-") }

// Revision resolves the worktree's current commit — the same as Base when the
// run committed nothing.
func (w Worktree) Revision(ctx context.Context) (string, error) {
	return HeadRevision(ctx, w.Path)
}

// Dirty reports whether the worktree has uncommitted changes, which is how a
// run that edited files but committed nothing is still recognised as having
// done something.
func (w Worktree) Dirty(ctx context.Context) (bool, error) {
	out, err := git(ctx, w.Path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Changed reports whether the run produced anything at all: a new commit, or
// uncommitted edits. This is a FACT about the repository, not a claim from the
// agent, and it is what separates a no_change outcome from a success.
func (w Worktree) Changed(ctx context.Context) (bool, error) {
	rev, err := w.Revision(ctx)
	if err != nil {
		return false, err
	}
	if rev != w.Base {
		return true, nil
	}
	return w.Dirty(ctx)
}

// Remove deletes the worktree and its branch.
//
// Called only for runs whose result nobody needs to look at. A failed or
// needs-attention run KEEPS its worktree: that checkout is the evidence, and
// deleting it to stay tidy destroys the thing a human is about to ask for.
func Remove(ctx context.Context, w Worktree) error {
	if w.Path == "" {
		return nil
	}
	if _, err := git(ctx, w.Repo, "worktree", "remove", "--force", w.Path); err != nil {
		// Fall back to removing the directory and pruning the registration, so a
		// half-removed worktree cannot wedge the next run of the same task.
		_ = os.RemoveAll(w.Path)
		_, _ = git(ctx, w.Repo, "worktree", "prune")
	}
	if w.Branch != "" {
		_, _ = git(ctx, w.Repo, "branch", "-D", w.Branch)
	}
	return nil
}

// Prune clears worktree registrations whose directories are gone.
func Prune(ctx context.Context, repo string) error {
	_, err := git(ctx, repo, "worktree", "prune")
	return err
}
