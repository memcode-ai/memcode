package taskrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/statedir"
	"github.com/memcode-ai/memcode/internal/taskgit"
)

// repoWithRemote makes a throwaway repo whose origin is a real bare repository,
// so tests exercise the ACTUAL push path rather than a mock of it. The
// worktree-escape bug in milestone 4 got through precisely because the real
// path was never executed.
func repoWithRemote(t *testing.T) (repo, origin string) {
	t.Helper()
	repo = repoT(t)
	origin = t.TempDir()
	if _, err := execGit(origin, "init", "-q", "--bare", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	// Give origin the same history, so it has a default branch to merge into.
	gitRun(t, repo, "remote", "add", "origin", origin)
	gitRun(t, repo, "push", "-q", "-u", "origin", "main")
	gitRun(t, repo, "fetch", "-q", "origin")
	return repo, origin
}

// repoT is repo() renamed for use from this file.
func repoT(t *testing.T) string { return repo(t) }

func remoteBranches(t *testing.T, origin string) []string {
	t.Helper()
	out, err := execGit(origin, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			got = append(got, l)
		}
	}
	return got
}

const publishing = `version: 1
name: publisher
description: Keep the catalog current.
instructions: change something
verify:
  commands:
    - "true"
`

// The whole milestone: a verified change becomes a commit on a NEW branch,
// pushed to the remote, with the run recording exactly what it created.
func TestPublishCommitsAndPushesANewBranch(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	repo, origin := repoWithRemote(t)

	run, err := runner(t, s, changing("updated the catalog")).
		Run(ctx, sample(t, publishing), repo, TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != OutcomeSuccess {
		t.Fatalf("outcome = %q (%s)", run.Outcome, run.Detail)
	}
	if run.CommitSHA == "" || !run.CreatedCommit {
		t.Error("the run must record the commit it created")
	}
	if run.ResultRev != run.CommitSHA || run.ResultRev == run.BaseRev {
		t.Errorf("result %s must be the new commit, not the base %s", run.ResultRev, run.BaseRev)
	}
	if !run.CreatedBranch {
		t.Error("the run must record that it created the branch")
	}
	if run.Remote != "origin" {
		t.Errorf("remote = %q, want origin", run.Remote)
	}

	// The branch is really on the remote, and main is untouched.
	branches := remoteBranches(t, origin)
	var found bool
	for _, b := range branches {
		if b == run.Branch {
			found = true
		}
	}
	if !found {
		t.Errorf("remote branches %v do not include %q", branches, run.Branch)
	}
	mainSHA, _ := gitOut(t, origin, "rev-parse", "main")
	if mainSHA == run.CommitSHA {
		t.Error("an autonomous run must never move the default branch")
	}

	// Published work is reachable from the remote, so the worktree is cleaned.
	if run.Worktree != "" {
		t.Errorf("a published run should clean its worktree, got %q", run.Worktree)
	}
}

// INVARIANT: no-change short-circuits. Nothing is committed, branched or pushed
// for a run that produced no diff, whatever pull_request says.
func TestNoChangePublishesNothing(t *testing.T) {
	for _, mode := range []string{"when_changes", "always"} {
		t.Run(mode, func(t *testing.T) {
			s := store(t)
			ctx := context.Background()
			repo, origin := repoWithRemote(t)
			before := remoteBranches(t, origin)

			tk := sample(t, "version: 1\nname: idle\ninstructions: look around\ngit:\n  pull_request: "+mode+"\n")
			run, err := runner(t, s, ok("Nothing needed changing.")).Run(ctx, tk, repo, TriggerManual, "")
			if err != nil {
				t.Fatal(err)
			}
			if run.Outcome != OutcomeNoChange {
				t.Fatalf("outcome = %q, want no_change", run.Outcome)
			}
			if run.CommitSHA != "" {
				t.Error("no_change must not produce a commit — an empty commit is a manufactured artifact")
			}
			if run.CreatedBranch || run.PRURL != "" {
				t.Error("no_change must not create a branch or a pull request")
			}
			if got := remoteBranches(t, origin); len(got) != len(before) {
				t.Errorf("remote branches changed from %v to %v", before, got)
			}
		})
	}
}

// INVARIANT: a verified failure is never published. Putting a branch whose
// tests fail in front of a reviewer presents it as ready.
func TestFailedVerificationPublishesNothing(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	repo, origin := repoWithRemote(t)
	before := remoteBranches(t, origin)

	tk := sample(t, "version: 1\nname: broken\ninstructions: x\nverify:\n  commands:\n    - \"false\"\n")
	run, err := runner(t, s, changing("looks good to me")).Run(ctx, tk, repo, TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", run.Outcome)
	}
	if run.CommitSHA != "" || run.CreatedBranch {
		t.Error("a failed run must publish nothing")
	}
	if got := remoteBranches(t, origin); len(got) != len(before) {
		t.Errorf("remote branches changed from %v to %v", before, got)
	}
	if run.Worktree == "" {
		t.Error("a failed run keeps its worktree — that is the evidence")
	}
}

// INVARIANT: push is new-branch-only. A remote branch this run did not create
// is refused outright rather than updated.
func TestPushRefusesABranchItDoesNotOwn(t *testing.T) {
	ctx := context.Background()
	repo, origin := repoWithRemote(t)

	// A human's branch, already on the remote under the name our run would use.
	wt, err := taskgit.Create(ctx, repo, "auto/collide")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "theirs.txt"), []byte("human work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := taskgit.Commit(ctx, wt, "human commit"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt.Path, "push", "-q", "origin", "auto/collide")
	theirSHA, _ := gitOut(t, origin, "rev-parse", "auto/collide")

	// A different run, same branch name, not owned by it.
	_, err = taskgit.Push(ctx, wt, "origin", "deadbeef", false)
	var notOurs taskgit.ErrBranchNotOurs
	if !asNotOurs(err, &notOurs) {
		t.Fatalf("push = %v, want ErrBranchNotOurs", err)
	}
	nowSHA, _ := gitOut(t, origin, "rev-parse", "auto/collide")
	if nowSHA != theirSHA {
		t.Error("the other branch must be untouched")
	}
}

// INVARIANT: never the default branch, under any name.
func TestPushRefusesTheDefaultBranch(t *testing.T) {
	ctx := context.Background()
	repo, _ := repoWithRemote(t)
	for _, branch := range []string{"main", "master", "HEAD"} {
		wt := taskgit.Worktree{Path: repo, Repo: repo, Branch: branch, Base: "x"}
		if _, err := taskgit.Push(ctx, wt, "origin", "x", true); err == nil ||
			!strings.Contains(err.Error(), "never publishes to the default branch") {
			t.Errorf("push to %q = %v, want a refusal", branch, err)
		}
	}
}

// INVARIANT: publishing is idempotent. A second attempt inside one run finds
// what the first did instead of duplicating it.
func TestPublishIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, origin := repoWithRemote(t)

	wt, err := taskgit.Create(ctx, repo, "auto/idem")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "work.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sha1, created1, err := taskgit.Commit(ctx, wt, "first")
	if err != nil || !created1 {
		t.Fatalf("first commit = %v, created=%v", err, created1)
	}
	// Committing again with nothing new staged returns the same commit, and
	// crucially does NOT make an empty second one.
	sha2, created2, err := taskgit.Commit(ctx, wt, "second")
	if err != nil {
		t.Fatal(err)
	}
	if sha2 != sha1 || created2 {
		t.Errorf("re-commit = %s created=%v, want the same %s and created=false", sha2, created2, sha1)
	}

	pushed1, err := taskgit.Push(ctx, wt, "origin", sha1, false)
	if err != nil || !pushed1 {
		t.Fatalf("first push = %v created=%v", err, pushed1)
	}
	pushed2, err := taskgit.Push(ctx, wt, "origin", sha1, true)
	if err != nil {
		t.Fatal(err)
	}
	if pushed2 {
		t.Error("re-pushing the same commit must report it created nothing")
	}
	if n := len(remoteBranches(t, origin)); n != 2 { // main + auto/idem
		t.Errorf("remote has %d branches, want no duplication", n)
	}
}

// INVARIANT: staging never sweeps up memcode's own local state.
func TestStagingExcludesMemcodeState(t *testing.T) {
	ctx := context.Background()
	repo, _ := repoWithRemote(t)
	wt, err := taskgit.Create(ctx, repo, "auto/staging")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt.Path, statedir.LegacyName, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(wt.Path, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".memcode/jobs/log", "internal noise\n")
	write("real-work.txt", "the actual change\n")

	if _, err := taskgit.Stage(ctx, wt); err != nil {
		t.Fatal(err)
	}
	staged, _ := gitOut(t, wt.Path, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "real-work.txt") {
		t.Errorf("the real change must be staged, got %q", staged)
	}
	if strings.Contains(staged, ".memcode") {
		t.Errorf("memcode's own state must never be committed, got %q", staged)
	}
}

// A local-only repository is a normal setup, not a failure. The work is
// committed and kept; nothing pretends a remote exists.
func TestLocalOnlyRepoCommitsAndKeepsTheWork(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	run, err := runner(t, s, changing("did the work")).
		Run(ctx, sample(t, publishing), repo(t), TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != OutcomeSuccess {
		t.Errorf("outcome = %q, want success — a missing remote is setup, not failure", run.Outcome)
	}
	if run.CommitSHA == "" {
		t.Error("the work must still be committed")
	}
	if run.CreatedBranch || run.PRURL != "" {
		t.Error("nothing can be published without a remote")
	}
	if run.Worktree == "" {
		t.Fatal("unpublished work must keep its worktree — this checkout is the only copy")
	}
	if _, err := os.Stat(run.Worktree); err != nil {
		t.Errorf("the retained worktree must exist: %v", err)
	}
	if !strings.Contains(run.Detail, "no \"origin\" remote") {
		t.Errorf("the run must say why nothing was pushed, got %q", run.Detail)
	}
}

// Provenance must be readable from the artifact itself, without opening
// memcode's database.
func TestProvenanceIsCarriedInTheArtifacts(t *testing.T) {
	p := taskgit.Provenance{
		Task: "upgrade-model-catalog", RunID: "run_123", TaskRevision: "sha256:abc",
		Base: "deadbeef", Occurrence: "cron:0 10 * * MON@2026-09-14T10:00:00Z",
		Verification: "ok   go test ./...", Description: "Refresh the model catalog.",
	}
	msg := taskgit.CommitMessage(p)
	for _, want := range []string{"upgrade-model-catalog", "run_123", "sha256:abc", "deadbeef"} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit message missing %q:\n%s", want, msg)
		}
	}
	if !strings.HasPrefix(msg, "Refresh the model catalog.") {
		t.Errorf("the subject should lead with the task's description:\n%s", msg)
	}

	body := taskgit.PRBody(p)
	for _, want := range []string{
		"upgrade-model-catalog", "run_123", "sha256:abc", "deadbeef",
		"cron:0 10 * * MON", "go test ./...", "cannot merge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
}

func TestPRBodySaysWhenNothingWasVerified(t *testing.T) {
	body := taskgit.PRBody(taskgit.Provenance{Task: "t", RunID: "r"})
	if !strings.Contains(body, "declared no verification commands") {
		t.Errorf("an unverified change must say so plainly:\n%s", body)
	}
}

func asNotOurs(err error, target *taskgit.ErrBranchNotOurs) bool {
	if e, ok := err.(taskgit.ErrBranchNotOurs); ok {
		*target = e
		return true
	}
	return false
}

// REGRESSION: a relative remote URL must still work. Push used to run in the
// linked worktree, where "../origin.git" resolves three levels below where the
// remote actually is; the failure surfaced as a bare "could not read from
// remote repository". Tests missed it because they wired origin by absolute
// path, which is the one case that happens to work either way.
func TestPushWorksWithARelativeRemote(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	origin := filepath.Join(parent, "origin.git")
	repo := filepath.Join(parent, "app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := execGit(parent, "init", "-q", "--bare", "-b", "main", "origin.git"); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "initial")
	// The relative form, exactly as a developer would write it.
	gitRun(t, repo, "remote", "add", "origin", "../origin.git")
	gitRun(t, repo, "push", "-q", "-u", "origin", "main")

	wt, err := taskgit.Create(ctx, repo, "auto/relative")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "new.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, _, err := taskgit.Commit(ctx, wt, "work")
	if err != nil {
		t.Fatal(err)
	}
	created, err := taskgit.Push(ctx, wt, "origin", sha, false)
	if err != nil {
		t.Fatalf("push with a relative remote: %v", err)
	}
	if !created {
		t.Error("the branch should have been created on the remote")
	}
	out, err := execGit(origin, "rev-parse", "auto/relative")
	if err != nil || strings.TrimSpace(out) != sha {
		t.Errorf("remote branch = %q (%v), want %s", strings.TrimSpace(out), err, sha)
	}
}
