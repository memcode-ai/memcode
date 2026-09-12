package taskrun

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/task"
	"github.com/memcode-ai/memcode/internal/taskgit"
)

// A responsibility is not always owned by one repository.
//
// memcode is the example that forced this: supported models are one concern
// implemented in two checkouts, and an automation for it that lived in
// whichever repo happened to be open would encode the wrong ownership and
// silently keep only half the product current.
//
// So a cross-project run is ONE logical run with per-project work under it, not
// two runs that happen to share a name. The distinction is not bookkeeping. It
// is the difference between
//
//	✓ succeeded — PR opened in repo A
//
// and
//
//	repo A's change is prepared; repo B could not be updated. Nothing published.
//
// when the two changes only make sense together. The parent owns the verdict,
// and the task's Coordination says which of those two readings is correct.

// projectWork is one project's contribution to a cross-project run.
type projectWork struct {
	Project string
	Res     Result
	WT      taskgit.Worktree
}

// execute dispatches on the shape of the responsibility.
func (r *Runner) execute(runCtx, storeCtx context.Context, run Run, t task.Task) Result {
	targets := t.TargetsFrom(run.Project)
	if len(targets) <= 1 {
		res, wt := r.workIn(runCtx, storeCtx, run, t, run.Project)
		if wt.Path != "" && res.OKOutcome() && res.Changed {
			if r.prepare(storeCtx, run, t, wt, &res) {
				r.publish(storeCtx, run, t, wt, &res)
			}
			if res.PRURL != "" {
				res.Summary = fmt.Sprintf("%s (%s)", res.Summary, res.PRURL)
			}
		}
		res = cleanup(storeCtx, res, wt)
		return finish(res)
	}
	return r.executeAcross(runCtx, storeCtx, run, t, targets)
}

// executeAcross runs one responsibility through several checkouts and returns a
// single verdict over all of them.
func (r *Runner) executeAcross(runCtx, storeCtx context.Context, run Run, t task.Task, targets []string) Result {
	works := make([]projectWork, 0, len(targets))

	// Sequential, deliberately. These projects were declared cross-cutting
	// because their changes relate to each other; running them concurrently
	// means the second agent cannot see what the first decided, which is the
	// one thing coordination is for.
	for _, p := range targets {
		sub := run
		sub.Project = p
		res, wt := r.workIn(runCtx, storeCtx, sub, t, p)
		works = append(works, projectWork{Project: p, Res: res, WT: wt})
		// A coordinated responsibility stops at the first project it cannot
		// satisfy: there is no point spending an agent on the rest when nothing
		// will be published either way.
		if t.Ownership.Coordination == task.CoordCoordinated && !res.OKOutcome() {
			break
		}
	}

	agg := aggregate(works, len(targets))

	// CROSS-PROJECT VERIFICATION. Per-project checks say each side is
	// internally fine; they cannot say the two sides now agree with each other,
	// which on a cross-cutting change is the failure that matters.
	if agg.OKOutcome() && len(t.Verify.Across) > 0 {
		checks, status := Verify(runCtx, anchorDir(works), t.Verify.Across, acrossEnv(works)...)
		if status == VerifyFail {
			agg.VerifyStatus = VerifyFail
			agg.Outcome = OutcomeNeedsAttention
			agg.Summary = "the projects no longer agree with each other"
		}
		agg.Checks = strings.TrimSpace(agg.Checks + "\n\nacross projects:\n" + Summarize(checks))
	}

	// PUBLICATION, in two phases, because only one of them can be undone.
	//
	// LOCAL FIRST. Committing is reversible and invisible to everyone else, so
	// every project commits before any project pushes. Without this ordering a
	// "coordinated" task could push repo A, then discover repo B cannot even
	// commit — with A already visible to the world.
	coordinated := t.Ownership.Coordination == task.CoordCoordinated
	ready := make([]int, 0, len(works))
	for i := range works {
		w := &works[i]
		if w.WT.Path == "" || !w.Res.OKOutcome() || !w.Res.Changed {
			continue
		}
		sub := run
		sub.Project = w.Project
		if r.prepare(storeCtx, sub, t, w.WT, &w.Res) {
			ready = append(ready, i)
		}
	}

	// THE GATE. A coordinated responsibility publishes only when the whole set
	// came through. Anything less and the remote phase never starts, so nothing
	// leaves this machine.
	gate := agg.OKOutcome()
	if coordinated && !allOK(works, len(targets)) {
		gate = false
		agg.Outcome = OutcomeNeedsAttention
		agg.Detail = appendLine(agg.Detail, "Nothing was published: this responsibility is coordinated, "+
			"so a change that only lands in some of its projects would leave the system in a state "+
			"neither half expects. The prepared work is committed in the worktrees listed below, and "+
			"a later run can pick it up from there.")
	}

	if gate {
		// REMOTE. From here it is irreversible and, across two git remotes,
		// genuinely not atomic: push A can succeed while push B fails. So the
		// claim changes the moment it stops being true — a partial publication
		// is reported as partial, never as the all-or-nothing it was until the
		// first push landed.
		for _, i := range ready {
			w := &works[i]
			sub := run
			sub.Project = w.Project
			r.publish(storeCtx, sub, t, w.WT, &w.Res)
		}
		if coordinated {
			if partial := unpublished(works, ready); len(partial) > 0 {
				agg.Outcome = OutcomeNeedsAttention
				agg.Detail = appendLine(agg.Detail, fmt.Sprintf(
					"PARTIALLY PUBLISHED. There is no transaction across git remotes, so this run "+
						"could not take back what it had already pushed when %s failed to publish. "+
						"The remote state is inconsistent and needs a human. Every project's commit "+
						"is kept, and re-running this task republishes only what is still missing.",
					strings.Join(partial, ", ")))
			}
		}
		agg = mergePublication(agg, works)
	}

	// Worktrees for published or clean work go; anything a human may need to
	// look at stays, and says where it is.
	for i := range works {
		works[i].Res = cleanup(storeCtx, works[i].Res, works[i].WT)
	}
	agg.Detail = strings.TrimSpace(agg.Detail + "\n\n" + perProject(works, t))
	if w, ok := anchorWork(works); ok {
		// The flat fields describe the anchor, which is where a single-project
		// reader (`task history`, the inbox row) will look.
		agg.Worktree, agg.Branch, agg.BaseRev, agg.ResultRev = w.Res.Worktree, w.Res.Branch, w.Res.BaseRev, w.Res.ResultRev
	}
	return finish(agg)
}

// aggregate reduces per-project results to the parent verdict.
//
// The parent is never better than its worst project, and a project that was
// never reached because an earlier one failed is itself a reason the whole
// thing needs attention rather than a silent omission.
func aggregate(works []projectWork, expected int) Result {
	out := Result{Outcome: OutcomeNoChange, ExecStatus: ExecCompleted, VerifyStatus: VerifySkipped}
	changedIn, failed := 0, 0
	for _, w := range works {
		if w.Res.Changed {
			changedIn++
		}
		if !w.Res.OKOutcome() {
			failed++
		}
		out.Outcome = worse(out.Outcome, w.Res.Outcome)
		out.ExecStatus = worseExec(out.ExecStatus, w.Res.ExecStatus)
		out.VerifyStatus = worseVerify(out.VerifyStatus, w.Res.VerifyStatus)
		out.Changed = out.Changed || w.Res.Changed
	}
	if len(works) < expected {
		out.Outcome = worse(out.Outcome, OutcomeNeedsAttention)
		out.Detail = appendLine(out.Detail, fmt.Sprintf(
			"Stopped after %d of %d projects.", len(works), expected))
	}
	switch {
	case failed > 0:
		out.Summary = fmt.Sprintf("%d of %d projects need attention", failed, expected)
	case changedIn > 0:
		out.Summary = fmt.Sprintf("updated %d of %d projects", changedIn, expected)
	default:
		out.Summary = fmt.Sprintf("nothing to change in %d projects", expected)
	}
	return out
}

// unpublished names the projects that were prepared and expected to publish but
// did not. Its emptiness is the only evidence that a coordinated publication
// actually held.
func unpublished(works []projectWork, ready []int) []string {
	var out []string
	for _, i := range ready {
		w := works[i]
		if w.Res.PRURL == "" && !w.Res.CreatedBranch {
			out = append(out, filepath.Base(w.Project))
		}
	}
	return out
}

func allOK(works []projectWork, expected int) bool {
	if len(works) < expected {
		return false
	}
	for _, w := range works {
		if !w.Res.OKOutcome() {
			return false
		}
	}
	return true
}

// mergePublication lifts what was actually published into the parent summary,
// so the headline names the outcome rather than a count.
func mergePublication(agg Result, works []projectWork) Result {
	var urls []string
	for _, w := range works {
		if w.Res.PRURL != "" {
			urls = append(urls, w.Res.PRURL)
		}
		agg.CreatedPR = agg.CreatedPR || w.Res.CreatedPR
		agg.CreatedBranch = agg.CreatedBranch || w.Res.CreatedBranch
		agg.CreatedCommit = agg.CreatedCommit || w.Res.CreatedCommit
	}
	if len(urls) == 0 {
		return agg
	}
	sort.Strings(urls)
	agg.PRURL = urls[0]
	agg.Summary = fmt.Sprintf("%s — %s", agg.Summary, strings.Join(urls, ", "))
	return agg
}

// perProject is the account a human reads when the headline is not enough.
func perProject(works []projectWork, t task.Task) string {
	var b strings.Builder
	b.WriteString("Per project:\n")
	for _, w := range works {
		fmt.Fprintf(&b, "  %s — %s: %s\n", filepath.Base(w.Project), w.Res.Outcome, firstLine(w.Res.Summary))
		if w.Res.PRURL != "" {
			fmt.Fprintf(&b, "      %s\n", w.Res.PRURL)
		}
		if w.Res.Worktree != "" {
			fmt.Fprintf(&b, "      work kept at %s\n", w.Res.Worktree)
		}
	}
	fmt.Fprintf(&b, "  (%s publication)\n", coordinationOf(t))
	return b.String()
}

func coordinationOf(t task.Task) string {
	if t.Ownership.Coordination == task.CoordCoordinated {
		return "all-or-nothing"
	}
	return "independent"
}

// anchorWork is the task's own project among the results.
func anchorWork(works []projectWork) (projectWork, bool) {
	if len(works) == 0 {
		return projectWork{}, false
	}
	return works[0], true
}

// anchorDir is where cross-project checks run: the anchor's worktree if it has
// one, so a consistency check reads the PROPOSED state rather than what is
// still on disk in the user's checkout.
func anchorDir(works []projectWork) string {
	w, ok := anchorWork(works)
	if !ok {
		return ""
	}
	if w.WT.Path != "" {
		return w.WT.Path
	}
	return w.Project
}

// acrossEnv hands a cross-project check the other projects' working copies.
// Without it such a check can only see the directory it runs in, which is
// exactly the thing it is not supposed to be limited to.
func acrossEnv(works []projectWork) []string {
	var dirs []string
	for _, w := range works {
		if w.WT.Path != "" {
			dirs = append(dirs, w.WT.Path)
			continue
		}
		dirs = append(dirs, w.Project)
	}
	return []string{"MEMCODE_TASK_PROJECTS=" + strings.Join(dirs, string(filepath.ListSeparator))}
}

// cleanup removes a worktree only when nobody will need to look at it.
func cleanup(ctx context.Context, res Result, wt taskgit.Worktree) Result {
	if wt.Path == "" || keepWorktree(res) {
		return res
	}
	_ = taskgit.Remove(ctx, wt)
	res.Worktree = ""
	return res
}

// Ordering over the verdict types, so a parent can take the worst of its
// children without anywhere having to enumerate pairs.
func rankOutcome(o Outcome) int {
	switch o {
	case OutcomeNoChange:
		return 0
	case OutcomeSuccess:
		return 1
	case OutcomeNeedsAttention:
		return 2
	case OutcomeBlocked:
		return 3
	}
	return 4 // failed, and anything unrecognised: assume the worst
}

func worse(a, b Outcome) Outcome {
	if rankOutcome(b) > rankOutcome(a) {
		return b
	}
	return a
}

func worseExec(a, b ExecutionStatus) ExecutionStatus {
	if a == b || b == ExecCompleted {
		return a
	}
	if a == ExecCompleted {
		return b
	}
	return a
}

func worseVerify(a, b VerificationStatus) VerificationStatus {
	switch {
	case a == VerifyFail || b == VerifyFail:
		return VerifyFail
	case a == VerifyPass || b == VerifyPass:
		return VerifyPass
	}
	return a
}
