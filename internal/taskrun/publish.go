package taskrun

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/task"
	"github.com/memcode-ai/memcode/internal/taskgit"
)

// Publication is TWO phases, and the split is load-bearing.
//
//	prepare   no-change check, grants, commit          — entirely LOCAL
//	publish   drift check, push, pull request          — REMOTE, irreversible
//
// A single-project run does both back to back and cannot tell the difference. A
// coordinated cross-project run must not: nothing may become visible to anyone
// until every project has passed, or "all or nothing" is a slogan rather than a
// property. Keeping the local half separately callable is what makes that
// achievable rather than aspirational (see executeAcross).
//
// The honest limit is stated where it bites: once the remote phase starts,
// there is no transaction across two git remotes. Push A can succeed and push B
// can fail, and when it does we say so rather than reporting the atomicity we
// no longer have.
//
// The order within each phase is fixed and every step is a real check against
// the repository. Nothing the model wrote reaches a git argument: it decided
// the diff; from here the plumbing is ours.

// prepare does the local half: commit the verified change, or explain why not.
// It reports whether the run has something a remote phase could publish.
func (r *Runner) prepare(ctx context.Context, run Run, t task.Task, wt taskgit.Worktree, res *Result) bool {

	// NO-CHANGE SHORT-CIRCUITS. A run that produced no diff publishes nothing —
	// not a branch, not an empty commit, not a pull request. `pull_request:
	// always` means always for a produced commit; manufacturing an empty
	// artifact to satisfy a configuration word helps nobody and leaves a repo
	// full of PRs that say nothing happened.
	if !res.Changed {
		return false
	}
	if t.Git.PullRequest == task.PRNever && !t.MayOpenPR() {
		// Nothing to publish to: the task keeps its work in the worktree.
		return false
	}
	if !t.HasGrant(task.GrantGitCommit) {
		return false
	}

	remote := t.Git.Remote
	if remote == "" {
		remote = taskgit.DefaultRemote
	}
	res.Remote = remote

	// COMMIT.
	prov := provenanceFor(run, t, wt, res)
	sha, createdCommit, err := taskgit.Commit(ctx, wt, taskgit.CommitMessage(prov))
	if err != nil {
		res.Outcome = OutcomeNeedsAttention
		res.Detail = appendLine(res.Detail, "Could not commit the change: "+err.Error())
		return false
	}
	res.CommitSHA, res.CreatedCommit, res.ResultRev = sha, createdCommit, sha

	if !t.HasGrant(task.GrantGitPushBranch) {
		res.Detail = appendLine(res.Detail, fmt.Sprintf(
			"Committed %s on %s in the worktree. This task's authority stops short of pushing, "+
				"so the branch stays local.", short(sha), wt.Branch))
		return false
	}
	if !taskgit.RemoteExists(ctx, wt.Repo, remote) {
		// A local-only repository. The work is committed and the worktree is
		// kept, which is everything that can be done; failing the run over a
		// missing remote would punish a normal local setup.
		res.Detail = appendLine(res.Detail, fmt.Sprintf(
			"Committed %s on %s. There is no %q remote, so nothing was pushed and no pull "+
				"request was opened. The branch is in the worktree: %s",
			short(sha), wt.Branch, remote, wt.Path))
		return false
	}
	return true
}

// provenanceFor is what a commit message and pull request body are built from:
// facts about the run, never model prose.
func provenanceFor(run Run, t task.Task, wt taskgit.Worktree, res *Result) taskgit.Provenance {
	prov := taskgit.Provenance{
		Task:         t.Name,
		RunID:        run.ID,
		TaskRevision: run.Revision,
		Base:         wt.Base,
		Verification: res.Checks,
		Description:  t.Description,
	}
	if !run.OccurredAt.IsZero() && run.TriggerKind != TriggerManual {
		prov.Occurrence = run.TriggerID
	}
	return prov
}

// publish does the remote half: drift check, push, pull request. Irreversible
// from the push onward, which is why nothing calls it until the whole set of
// work it belongs to has been prepared.
func (r *Runner) publish(ctx context.Context, run Run, t task.Task, wt taskgit.Worktree, res *Result) {
	sha := res.CommitSHA
	remote := res.Remote
	prov := provenanceFor(run, t, wt, res)
	// DRIFT. Refresh the remote view first, or the checks below judge a stale
	// picture of what the branch would merge into.
	_ = taskgit.Fetch(ctx, wt.Repo, remote)
	def := taskgit.DefaultBranch(ctx, wt.Repo, remote)
	if conflicts, ok := taskgit.MergeConflicts(ctx, wt.Repo, remote, def, sha); ok && conflicts {
		// The base moved under the run and the result no longer merges. We do
		// NOT rebase: rewriting history under an unattended run is how work
		// quietly disappears, and a machine guessing at a conflict resolution
		// nobody asked for is worse than stopping. The commit is kept, the
		// branch is not published, and a human decides.
		res.Outcome = OutcomeNeedsAttention
		res.Detail = appendLine(res.Detail, fmt.Sprintf(
			"The base moved while this ran and %s no longer merges cleanly into %s/%s. "+
				"Nothing was rebased and nothing was pushed. The commit is in the worktree: %s",
			short(sha), remote, def, wt.Path))
		return
	}

	// PUSH — new branch only.
	createdBranch, err := taskgit.Push(ctx, wt, remote, sha, res.CreatedBranch)
	if err != nil {
		var notOurs taskgit.ErrBranchNotOurs
		if errors.As(err, &notOurs) {
			res.Outcome = OutcomeNeedsAttention
			res.Detail = appendLine(res.Detail, notOurs.Error()+
				". The commit is in the worktree: "+wt.Path)
			return
		}
		res.Outcome = OutcomeNeedsAttention
		res.Detail = appendLine(res.Detail, "Could not push the branch: "+err.Error())
		return
	}
	res.CreatedBranch = res.CreatedBranch || createdBranch

	// PULL REQUEST.
	if t.Git.PullRequest == task.PRNever || !t.HasGrant(task.GrantGitHubOpenPR) {
		res.Detail = appendLine(res.Detail, fmt.Sprintf(
			"Pushed %s to %s/%s. No pull request was opened.", short(sha), remote, wt.Branch))
		return
	}
	number, url, createdPR, err := taskgit.OpenPR(ctx, wt.Path, wt.Branch, def,
		taskgit.PRTitle(prov), taskgit.PRBody(prov))
	if err != nil {
		pushed := fmt.Sprintf("Pushed %s to %s/%s", short(sha), remote, wt.Branch)
		var unsupported taskgit.ErrPRUnsupported
		if errors.As(err, &unsupported) {
			// Pull requests are not possible in this setup at all. The change is
			// pushed and reachable, so this is a fact to state once per run, not
			// a problem with the run — flagging it every time would train people
			// to ignore needs_attention.
			res.Detail = appendLine(res.Detail, pushed+". No pull request: "+unsupported.Reason)
			return
		}
		// A real failure to open one: the work is safe but the review artifact
		// someone expected is missing.
		res.Outcome = OutcomeNeedsAttention
		res.Detail = appendLine(res.Detail, fmt.Sprintf("%s, but could not open a pull request: %v", pushed, err))
		return
	}
	res.PRNumber, res.PRURL, res.CreatedPR = number, url, createdPR
	verb := "Opened"
	if !createdPR {
		verb = "Reused"
	}
	res.Detail = appendLine(res.Detail, fmt.Sprintf("%s %s (%s → %s/%s)", verb, url, wt.Branch, remote, def))
}

// keepWorktree reports whether a run's worktree must survive.
//
// Published work is reachable from the remote, so a successful published run
// can be cleaned up. Anything a human may need to look at — a failure, a
// conflict, an unpushed commit — keeps its checkout, because that directory is
// the evidence and tidiness is not worth destroying it.
func keepWorktree(res Result) bool {
	if !res.OKOutcome() {
		return true
	}
	if !res.Changed {
		return false // nothing to lose
	}
	// Changed but never published: this checkout is the ONLY copy of the work.
	// Deleting it because the outcome was nominally fine would throw the change
	// away, which is the worst thing this code could do.
	return res.PRURL == "" && !res.CreatedBranch
}

func appendLine(s, line string) string {
	if strings.TrimSpace(s) == "" {
		return line
	}
	return s + "\n\n" + line
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
