package taskrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/jobs"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/runtimes"
	"github.com/memcode-ai/memcode/internal/task"
	"github.com/memcode-ai/memcode/internal/taskgit"
)

// Runner turns a task definition into a durable, executed Run.
//
// The order matters and is the point of the whole design:
//
//	freeze inputs -> create the row (claims the occurrence) -> claim the work
//	-> execute -> persist the outcome
//
// The row exists before any work starts, so a duplicate dispatch is rejected by
// the ledger rather than by hoping two executions do not overlap.
type Runner struct {
	Store *Store
	// Spawn runs the agent and blocks until it finishes. Injectable so tests
	// exercise the whole lifecycle — including crashes — without a model call.
	Spawn SpawnFunc
	// HeartbeatEvery bounds how stale a live run's heartbeat can get.
	HeartbeatEvery time.Duration
	// Auth and Available answer "what is permitted" and "what is present".
	// Injectable so a test's answer does not depend on the developer's laptop.
	Auth      AuthSource
	Available func() []string
}

// SpawnFunc executes a task's work and reports what happened.
type SpawnFunc func(ctx context.Context, req SpawnRequest) (SpawnResult, error)

// SpawnRequest is everything the executor needs, taken from the FROZEN run
// rather than from the task file, so a mid-run YAML edit cannot reach it.
type SpawnRequest struct {
	RunID string
	// Project is the repository that owns the run's bookkeeping — its job log
	// outlives a disposable worktree because of this.
	Project string
	// WorkDir is where the work actually happens: the isolated worktree, or the
	// project itself when there is nothing to isolate.
	WorkDir      string
	Instructions string
	Mode         permissions.Mode
	ReadOnly     bool
	// Env selects the resolved runtime for the child.
	Env []string
	// DenyTools and DenyCommands are the capability ceiling, projected from the
	// task's grants. Both are real restrictions on the child.
	DenyTools    []string
	DenyCommands []string
	Timeout      time.Duration
}

// SpawnResult is the executor's report.
type SpawnResult struct {
	Text     string
	LogPath  string
	ExitCode int
}

// NewRunner builds a runner backed by real detached memcode processes.
func NewRunner(store *Store, auth AuthSource) *Runner {
	return &Runner{Store: store, Spawn: spawnJob, HeartbeatEvery: 20 * time.Second, Auth: auth}
}

// Freeze captures a task's execution inputs as a Run, resolving everything that
// could otherwise drift: the definition's revision AND full text, the absolute
// project path, the expanded authority, the runtime, the limits.
//
// Nothing downstream reads the task file again. That is what makes a run mean
// one fixed thing forever, and it is why the definition is stored whole rather
// than by reference — a run stays readable after its file is edited or deleted.
func Freeze(t task.Task, root, triggerKind, triggerID string, now time.Time) (Run, error) {
	return FreezeAt(t, root, triggerKind, triggerID, now, now, 0)
}

// FreezeAt is Freeze with the LOGICAL occurrence this run stands for, and the
// size of the backlog collapsed into it. A run_once recovery of Monday's
// occurrence executed on Wednesday is occurredAt Monday: the ledger then says
// which firing was recovered, instead of only when someone got round to it.
func FreezeAt(t task.Task, root, triggerKind, triggerID string, occurredAt, now time.Time, backlog int) (Run, error) {
	return FreezeWith(t, root, triggerKind, triggerID, occurredAt, now, backlog, runtimes.Resolution{})
}

// FreezeWith is FreezeAt plus the resolved runtime, frozen with everything else.
func FreezeWith(t task.Task, root, triggerKind, triggerID string, occurredAt, now time.Time,
	backlog int, rt runtimes.Resolution,
) (Run, error) {
	rev, err := t.Revision()
	if err != nil {
		return Run{}, err
	}
	project, err := t.ResolveProject(root)
	if err != nil {
		return Run{}, err
	}
	grants, err := t.Grants()
	if err != nil {
		return Run{}, err
	}
	def, err := task.Marshal(t)
	if err != nil {
		return Run{}, err
	}
	gs := make([]string, len(grants))
	for i, g := range grants {
		gs[i] = string(g)
	}
	return Run{
		ID:               NewID(now),
		Task:             t.Name,
		Revision:         rev,
		Definition:       string(def),
		TriggerKind:      triggerKind,
		TriggerID:        triggerID,
		Project:          project,
		Grants:           gs,
		RuntimeRequested: string(rt.RequestedStrategy),
		RuntimeResolved:  rt.Runtime,
		ModelRequested:   rt.RequestedModel,
		ModelResolved:    rt.Model,
		CredSource:       rt.CredentialSource,
		AuthID:           rt.AuthID,
		AuthScope:        string(rt.AuthScope),
		Timeout:          t.Timeout(),
		MaxCostUSD:       t.Limits.MaxCostUSD,
		StartedAt:        now,
		OccurredAt:       occurredAt,
		Backlog:          backlog,
		Seen:             SeenUnseen,
	}, nil
}

// executionContract rides every autonomous run, ahead of the task's own
// instructions.
//
// A scheduled task runs against a repository that has moved since anyone looked
// at it: files renamed, packages restructured, build and test commands changed,
// the thing it was written to maintain now living somewhere else. The failure
// mode is not a crash — it is a run that confidently does the wrong thing
// because it assumed a layout that no longer exists, and nobody was watching.
//
// So the contract is explicit: the goal is durable, everything else is a
// hypothesis to re-check.
const executionContract = `You are running unattended. Nobody will answer a question, and nobody is
watching the output.

The repository has probably changed since this task was written. Before acting:
- work out the CURRENT state rather than assuming a remembered one
- do not trust remembered paths, commands or file layouts until you have checked them
- if something referenced here no longer exists, find its equivalent instead of failing or
  inventing one
- treat any recorded approach below as evidence that it worked once, not as instructions
- if the goal turns out to be already satisfied, change nothing and say so

Judge yourself against the GOAL and the verification commands, never against whether the
recorded steps ran. If you cannot achieve the goal safely, stop and report why — an honest
"this needs a human" is worth more than a plausible wrong change nobody reviewed.

End your final message with exactly one line saying what this means for FUTURE runs:

  ESCALATION: continue        nothing to raise, or you fixed it yourself
  ESCALATION: retry_later     a passing problem — an outage, a flaky network, a locked file
  ESCALATION: needs_attention someone should look at this run, but next week is still worth running
  ESCALATION: pause_task <why>  stop running this until a person decides

Choose pause_task when continuing would need you to invent intent the task does not contain,
take authority it was not given, or make a consequential architectural choice on someone's
behalf — a dependency that only upgrades via one of two incompatible migrations, a goal that
now lives in a repository outside your boundary, a check that has become impossible to
evaluate. Those conditions do not clear on their own, and running into the same wall every
week is worse than stopping: it buries a real decision under identical failures nobody reads.

---

`

// boundaryContract is where self-healing stops.
//
// A run may adapt to anything it finds INSIDE the projects it was given: files
// move, packages get renamed, build commands change, the code it maintains ends
// up three directories over. That is drift, and handling it is the point.
//
// Reaching into a project nobody approved is not drift. It is the task growing
// its own authority, unattended, on the strength of its own reasoning about
// where the work now lives — which is exactly the decision a person is supposed
// to make. So the boundary is stated to the agent as a hard edge with a defined
// exit: stop, and say what it found.
const boundaryContract = `Your approved boundary is the projects listed above and nothing else.

Inside them, adapt freely. Outside them, do not act at all: if satisfying the goal now seems
to require changing a repository that is not listed, make no change there, do not clone it,
and do not work around it. Say clearly that this NEEDS ATTENTION, name the repository, and
explain what appears to have moved. Someone will widen the task if that is right.

`

// instructionsFor composes what the agent actually receives: the drift
// contract, the task's durable goal, and any known-good approach clearly marked
// as a hint.
func instructionsFor(t task.Task, project string, targets []string) string {
	var b strings.Builder
	b.WriteString(executionContract)
	// A cross-project run works one checkout at a time, and the agent has to
	// know which share of the responsibility is its own — otherwise it reads a
	// goal phrased over two repositories, finds half of it missing here, and
	// either fails or goes looking outside the worktree it was given.
	if len(targets) > 1 {
		fmt.Fprintf(&b, `This responsibility spans several projects: %s.

You are working on %s ONLY. Do this project's share of the goal, here, and leave the others
alone; they get their own turn. Whether the projects still agree with each other is checked
separately once every project has been through.

`, strings.Join(targets, ", "), project)
		if d := strings.TrimSpace(t.Ownership.Responsibility); d != "" {
			fmt.Fprintf(&b, "What this task is responsible for, whatever the current layout: %s\n\n", d)
		}
		b.WriteString(boundaryContract)
	}
	b.WriteString(t.Instructions)
	if kg := strings.TrimSpace(t.Execution.KnownGood); kg != "" {
		b.WriteString("\n\n--- an approach that worked when this task was created ---\n")
		b.WriteString(kg)
		b.WriteString("\n(Evidence, not authority. Check it still applies.)")
	}
	return b.String()
}

// modeFor picks the permission mode a run executes under. Both tiers use
// ModeAuto: Safe and Medium run unattended, Dangerous and catastrophic still
// prompt, find nobody, and are therefore refused.
//
// Read-only is deliberately NOT expressed as a stricter mode. ModeAsk looks like
// it should work — no human means every prompt is a denial — but it does not,
// and a real run proved it: a read_only task was asked to create a file and did,
// logging "auto-allowed" in ask mode. The authorization judge downgrades a
// prompt to an allow when the request plainly asked for the action, which is a
// good rule for an interactive session (it catches an agent freelancing PAST
// what the user wanted) and exactly backwards for a task, whose instructions
// always authorize the task's own work.
//
// So authority is enforced as CAPABILITY instead: see readOnlyFor. A tool the
// child was never given cannot be argued for.
func modeFor(task.Task) permissions.Mode { return permissions.ModeAuto }

// Authorizations supplies the machine's recorded runtime permissions.
// Injectable so tests do not depend on whatever is installed on the developer's
// laptop, and so the daemon and the CLI read the same store.
type AuthSource func() runtimes.Authorizations

// resolveRuntime decides where this task's inference runs, and refuses to
// proceed when nothing is both authorized and capable. Resolution happens
// BEFORE the run row is created, so the record carries the decision rather than
// the run discovering it halfway through.
func (r *Runner) resolveRuntime(t task.Task, runID string) (runtimes.Resolution, error) {
	auth := runtimes.Authorizations(nil)
	if r.Auth != nil {
		auth = r.Auth()
	}
	avail := runtimes.Detect()
	if r.Available != nil {
		avail = r.Available()
	}
	return runtimes.Resolve(runtimes.Policy{
		Strategy: runtimes.Strategy(t.Runtime.Strategy),
		Allowed:  t.Runtime.Allowed,
		Model:    t.Runtime.Model,
		Fallback: t.Runtime.Fallback,
	}, runtimes.Env{
		Available:      avail,
		Authorizations: auth,
		Task:           t.Name,
		Run:            runID,
	})
}

// Start freezes, records and claims a run without executing it. Returns
// ErrOccupied when the occurrence already belongs to another run.
func (r *Runner) Start(ctx context.Context, t task.Task, root, triggerKind, triggerID string, now time.Time) (Run, error) {
	if !t.IsEnabled() {
		return Run{}, fmt.Errorf("task %q is disabled", t.Name)
	}
	rt, err := r.resolveRuntime(t, "")
	if err != nil {
		return Run{}, err
	}
	frozen, err := FreezeWith(t, root, triggerKind, triggerID, now, now, 0, rt)
	if err != nil {
		return Run{}, err
	}
	return r.Store.Create(ctx, frozen)
}

// Execute claims and runs an already-created run to completion, persisting the
// outcome. Safe to call only once per run; the conditional claim enforces that.
func (r *Runner) Execute(ctx context.Context, run Run, t task.Task) (Run, error) {
	claimed, err := r.Store.Claim(ctx, run.ID, time.Now())
	if err != nil {
		return run, err
	}
	if !claimed {
		// Another process got here first, or this run is already finished.
		return r.Store.Get(ctx, run.ID)
	}

	// Exclusive use of the resource this run mutates. Taken AFTER the claim so a
	// run that loses the claim never touches the lease, and released on every
	// path out — including a panic — so a crash is the only way to leave one
	// behind, which the expiry rule then handles.
	keys := LeaseKeys(t, run.Project)
	var taken []string
	release := func() {
		for _, k := range taken {
			_ = r.Store.Release(context.WithoutCancel(ctx), k, run.ID)
		}
	}
	defer release()
	for _, key := range keys {
		if err := r.Store.Acquire(ctx, key, run.ID, run.Task, time.Now()); err != nil {
			var held ErrLeaseHeld
			if errors.As(err, &held) {
				// Partial acquisition is released by the deferred call, so a run
				// that loses a race leaves nothing locked behind it.
				_ = r.Store.Finish(ctx, run.ID, OutcomeBlocked,
					"another run holds this project",
					fmt.Sprintf("Waiting on run %s (task %s) for %s. Mutating tasks in one project run "+
						"one at a time; read-only tasks are free to overlap.", held.Holder.RunID, held.Holder.Task, key),
					"", time.Now())
				return r.Store.Get(ctx, run.ID)
			}
			return run, err
		}
		taken = append(taken, key)
	}

	// Heartbeat for as long as the work runs. Its absence is how Reconcile
	// distinguishes a crashed run from a slow one, and the same beat keeps the
	// lease alive so the two notions of "that process is gone" cannot disagree.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go r.heartbeat(hbCtx, run.ID, taken...)

	runCtx := ctx
	if run.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, run.Timeout)
		defer cancel()
	}

	res := r.execute(runCtx, ctx, run, t)
	if ferr := r.Store.FinishResult(ctx, run.ID, res, time.Now()); ferr != nil {
		return run, ferr
	}
	// SUSPEND, if this run concluded the condition will still be here next
	// time. Recorded after the run so the pause can point at the evidence, and
	// never for a manual run: a person is already here, watching, and can
	// decide for themselves whether to try again.
	if res.Escalation == EscalatePause && run.TriggerKind != TriggerManual {
		_ = r.Store.Pause(ctx, Pause{
			Task: run.Task, Project: run.Project, RunID: run.ID,
			Reason: firstNonEmpty(res.EscalationWhy, res.Summary), Since: time.Now(),
		})
	}
	return r.Store.Get(ctx, run.ID)
}

// execute is the phased body of a run: isolate, work, verify, classify.
//
//	isolate   a fresh worktree, so nothing touches the user's checkout
//	work      the agent, with only the capabilities its grants project
//	verify    the task's own checks, run by US and judged on exit codes
//	classify  the outcome, derived from facts
//
// The phases are separate because their verdicts are separate. The agent can
// complete cleanly and the tests can still fail; the run can be blocked before
// the agent ever starts. Collapsing those into one signal loses exactly the
// information a human needs when something goes wrong.
// The return value is NAMED so the cleanup defer can clear the recorded
// worktree path: removing the directory while still reporting where it was
// would leave every successful run pointing at somewhere that does not exist.
// workIn is one project's share of a run: isolate, work, verify, classify.
// It deliberately stops short of publishing and of cleaning up its worktree,
// because on a cross-project run neither decision belongs to a single project
// (see executeAcross). The caller owns the returned worktree either way.
func (r *Runner) workIn(runCtx, storeCtx context.Context, run Run, t task.Task, project string) (res Result, wt taskgit.Worktree) {
	res = Result{Outcome: OutcomeFailed, ExecStatus: ExecFailed, VerifyStatus: VerifySkipped}

	cap, err := t.Capability()
	if err != nil {
		res.Summary, res.Detail = "capability projection failed", err.Error()
		res.ExecStatus = ExecBlocked
		res.Outcome = OutcomeBlocked
		return res, wt
	}

	// memcode's own state must not turn up in the user's `git status`. An
	// interactive session does this at launch; an unattended run may be the
	// first thing that ever touches this repo, so it does it too. Idempotent,
	// and a no-op when .memcode does not exist.
	config.EnsureGitignore(project)

	// ISOLATE. Every autonomous run works somewhere that is not the user's
	// checkout: a mutating one needs a branch to build on, and a read-only one
	// still needs its test caches to land off the tree someone is sitting in.
	dir := project
	if cap.ProtectProject {
		if !taskgit.IsRepo(storeCtx, project) {
			if cap.Mutating {
				res.Summary = "project is not a git repository"
				res.Detail = "A task that changes code runs in an isolated worktree, which needs git. " +
					"Initialize the repository, or set autonomy.level: read_only."
				res.ExecStatus, res.Outcome = ExecBlocked, OutcomeBlocked
				return res, wt
			}
			// Read-only work in a non-repo: nothing to isolate into, and nothing
			// it is allowed to change anyway.
		} else {
			branch := taskgit.BranchName(t.Git.Branch, t.Name, run.ID, time.Now())
			created, cerr := taskgit.Create(storeCtx, project, branch)
			if cerr != nil {
				res.Summary = "could not isolate the run"
				res.Detail = cerr.Error()
				res.ExecStatus, res.Outcome = ExecBlocked, OutcomeBlocked
				return res, wt
			}
			wt = created
			dir = wt.Path
			res.Worktree, res.Branch, res.BaseRev = wt.Path, wt.Branch, wt.Base
			res.ResultRev = wt.Base
		}
	}

	// RUNTIME PREFLIGHT. The resolved backend must actually work BEFORE the
	// agent starts, because that is the only moment a substitution is honest.
	// Once a runtime has run the task, any later failure belongs to the task.
	if why, ok := runtimeUsable(run); !ok {
		next := r.nextRuntime(t, run, why)
		if next == "" {
			res.Summary = "no usable runtime"
			res.Detail = appendLine(res.Detail, fmt.Sprintf(
				"Runtime %q could not be used (%s) and no authorized fallback remains.", run.RuntimeResolved, why))
			res.ExecStatus, res.Outcome = ExecBlocked, OutcomeBlocked
			return res, wt
		}
		res.Detail = appendLine(res.Detail, fmt.Sprintf(
			"Runtime %q was unusable (%s); fell back to %q.", run.RuntimeResolved, why, next))
		run.RuntimeResolved = next
		if rt, ok2 := runtimes.Get(next); ok2 {
			run.CredSource = rt.CredentialSource
		}
	}

	// WORK.
	spawn, serr := r.Spawn(runCtx, SpawnRequest{
		RunID:        run.ID,
		Project:      project,
		WorkDir:      dir,
		Instructions: instructionsFor(t, project, t.TargetsFrom(run.Project)),
		Mode:         modeFor(t),
		ReadOnly:     false,
		Env:          runtimeEnv(run),
		DenyTools:    cap.DenyTools,
		DenyCommands: cap.DenyCommands,
		Timeout:      run.Timeout,
	})
	res.LogPath = spawn.LogPath
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		res.ExecStatus = ExecTimedOut
		res.Summary, res.Detail = "timed out", "The run exceeded its limits.timeout and was stopped."
	case serr != nil:
		res.ExecStatus = ExecFailed
		res.Summary, res.Detail = "the run could not complete", serr.Error()
	case spawn.ExitCode != 0:
		res.ExecStatus = ExecFailed
		res.Summary, res.Detail = fmt.Sprintf("exited %d", spawn.ExitCode), clip(spawn.Text, 4000)
	default:
		res.ExecStatus = ExecCompleted
		res.Summary, res.Detail = firstLine(spawn.Text), clip(spawn.Text, 4000)
	}

	// What actually changed is read from the REPOSITORY, not from the report.
	if wt.Path != "" {
		if rev, rerr := wt.Revision(storeCtx); rerr == nil {
			res.ResultRev = rev
		}
		if ch, cerr := wt.Changed(storeCtx); cerr == nil {
			res.Changed = ch
		}
	}

	// VERIFY. Only if the agent got that far; verifying after a timeout tells
	// nobody anything and costs a test suite.
	if res.ExecStatus == ExecCompleted {
		checks, status := Verify(runCtx, dir, t.Verify.Commands)
		res.VerifyStatus = status
		res.Checks = Summarize(checks)
	}

	// CLASSIFY, from facts. The agent's only entry point is raising
	// needs_attention, which can make the verdict more cautious and never less.
	esc, why := ParseEscalation(spawn.Text)
	res.Escalation, res.EscalationWhy = esc, why
	res.Outcome = Decide(res.ExecStatus, res.VerifyStatus, res.Changed,
		mentionsNeedsAttention(spawn.Text) || esc == EscalateAttention || esc == EscalatePause)
	if res.VerifyStatus == VerifyFail {
		res.Summary = "verification failed"
	}

	return res, wt
}

// finish is the per-project tail shared by both shapes: fold the checks into
// the detail and say where the evidence was left.
func finish(res Result) Result {
	if strings.TrimSpace(res.Summary) == "" {
		// An agent that finished without a closing line still needs a legible
		// row in the history and the inbox.
		res.Summary = defaultSummary(res)
	}
	if res.Checks != "" {
		res.Detail = strings.TrimSpace(res.Detail + "\n\n" + res.Checks)
	}
	if res.Worktree != "" && !res.OKOutcome() {
		res.Detail = strings.TrimSpace(res.Detail + "\n\nWorktree kept for inspection: " + res.Worktree)
	}
	return res
}

// defaultSummary describes a run that said nothing about itself, from what it
// actually did.
func defaultSummary(res Result) string {
	switch {
	case res.PRURL != "":
		return "opened " + res.PRURL
	case res.CommitSHA != "" && res.CreatedBranch:
		return fmt.Sprintf("pushed %s to %s", short(res.CommitSHA), res.Branch)
	case res.CommitSHA != "":
		return fmt.Sprintf("committed %s on %s", short(res.CommitSHA), res.Branch)
	case res.Changed:
		return "changed files"
	case res.Outcome == OutcomeNoChange:
		return "nothing to change"
	}
	return string(res.Outcome)
}

// OKOutcome reports whether the result is one nobody needs to look at.
func (r Result) OKOutcome() bool {
	return r.Outcome == OutcomeSuccess || r.Outcome == OutcomeNoChange
}

// Run is the whole loop: freeze, record, claim, execute, persist.// Run is the whole loop: freeze, record, claim, execute, persist.
func (r *Runner) Run(ctx context.Context, t task.Task, root, triggerKind, triggerID string) (Run, error) {
	run, err := r.Start(ctx, t, root, triggerKind, triggerID, time.Now())
	if err != nil {
		return Run{}, err
	}
	return r.Execute(ctx, run, t)
}

func (r *Runner) heartbeat(ctx context.Context, id string, leaseKeys ...string) {
	every := r.HeartbeatEvery
	if every <= 0 {
		every = 20 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			// Detached from ctx on purpose: a cancelled run should still record
			// its last heartbeat rather than look stale to Reconcile.
			hbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = r.Store.Heartbeat(hbCtx, id, now)
			for _, k := range leaseKeys {
				_ = r.Store.Renew(hbCtx, k, id, now)
			}
			cancel()
		}
	}
}

// mentionsNeedsAttention is the agent's ONE input into the verdict: it may raise
// needs_attention, making the outcome more cautious. It has no path to lower
// one — success and no_change are derived from execution status, verification
// exit codes, and whether the repository actually changed.
//
// Reading prose at all is a compromise, kept deliberately narrow. Everything
// that decides whether a run WORKED is now a fact; this only decides whether to
// escalate something that already worked.
//
// The boundary case rides on this too: boundaryContract tells a run that found
// its work outside the approved projects to say NEEDS ATTENTION, which lands
// here. No new marker was added for it — a boundary word like "outside" appears
// in ordinary prose constantly, and a matcher that fires on it would escalate
// every run that mentioned a file outside a package.
func mentionsNeedsAttention(s string) bool {
	l := strings.ToLower(s)
	for _, p := range []string{"needs attention", "needs your", "requires a human",
		"could not decide", "ambiguous",
		// A run refused a capability did not do its job, however calmly it says
		// so. Matched broadly on purpose: this is how a task whose authority is
		// too narrow for its instructions gets noticed instead of reading fine.
		"denied", "blocked", "not permitted", "unable to"} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clip(s, 200)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// runtimeUsable checks that the chosen backend can actually be reached, before
// any work starts. This is the ONLY place a runtime substitution may happen.
//
// After the agent has run, a failure belongs to the task, not the runtime.
// Handing the same codebase to a different model because the tests failed is
// not fallback — it is rerolling until something agrees, which converts one
// honest failure into a search for a favourable opinion.
func runtimeUsable(run Run) (string, bool) {
	if run.CredSource == "" {
		return "", true // the hosted gateway: reachability is the run's problem, not a selection one
	}
	if _, ok := provider.ResolveCredentialSource(run.CredSource); !ok {
		return "the credential could not be resolved (signed out of the host tool, or the token expired)", false
	}
	return "", true
}

// nextRuntime returns the first authorized, capable fallback, or "".
func (r *Runner) nextRuntime(t task.Task, run Run, _ string) string {
	auth := runtimes.Authorizations(nil)
	if r.Auth != nil {
		auth = r.Auth()
	}
	avail := runtimes.Detect()
	if r.Available != nil {
		avail = r.Available()
	}
	chain := runtimes.Chain(runtimes.Policy{
		Strategy: runtimes.Strategy(t.Runtime.Strategy),
		Allowed:  t.Runtime.Allowed,
		Model:    t.Runtime.Model,
		Fallback: t.Runtime.Fallback,
	}, runtimes.Env{Available: avail, Authorizations: auth, Task: t.Name, Run: run.ID},
		run.RuntimeResolved)
	if len(chain) == 0 {
		return ""
	}
	return chain[0]
}

// runtimeEnv translates the frozen runtime decision into the environment the
// child selects its backend from.
//
// The credential source is passed EXPLICITLY rather than letting the child
// discover one: a detached process inheriting whatever ambient credential
// happens to be configured would run wherever the machine felt like, which is
// the opposite of a recorded, authorized decision.
func runtimeEnv(run Run) []string {
	var env []string
	// Always set, even to empty: an empty value means "no subscription source",
	// which must override an inherited one rather than fall through to it.
	env = append(env, provider.EnvCredentialSource+"="+run.CredSource)
	if run.ModelResolved != "" {
		env = append(env, provider.EnvEndpointModel+"="+run.ModelResolved)
	}
	return env
}

// jobSpec builds the child's spawn spec. Split out and tested directly because
// the real spawn path is the ONE part of a run that an injected fake executor
// cannot cover — and a missing field here is invisible until it matters. It was:
// WorkDir went unset for a while, so the child ran in the user's checkout and
// the agent's file writes escaped the worktree while every test still passed.
func jobSpec(req SpawnRequest, workDir string) jobs.SpawnSpec {
	return jobs.SpawnSpec{
		// Root owns the bookkeeping (job dir, meta, log) so a log outlives a
		// disposable worktree; WorkDir is where the process actually runs, which
		// is what keeps the agent's file writes inside the isolation.
		Root:         req.Project,
		WorkDir:      workDir,
		Task:         req.Instructions,
		Mode:         string(req.Mode),
		RunID:        req.RunID,
		ReadOnly:     req.ReadOnly,
		ToolPolicy:   jobs.ToolPolicy{Disabled: req.DenyTools},
		DenyCommands: req.DenyCommands,
		Env:          req.Env,
		ReportBack:   true,
	}
}

// spawnJob runs the work as a detached memcode child and waits for it, reusing
// the job machinery the gateway already drives (liveness by pid AND start-time
// signature, a heartbeated meta.json, a durable log).
func spawnJob(ctx context.Context, req SpawnRequest) (SpawnResult, error) {
	workDir := req.WorkDir
	if workDir == "" {
		workDir = req.Project
	}
	if _, err := os.Stat(workDir); err != nil {
		return SpawnResult{}, fmt.Errorf("working directory %s is not reachable: %w", workDir, err)
	}
	job, err := jobs.SpawnWithSpec(jobSpec(req, workDir))
	if err != nil {
		return SpawnResult{}, err
	}
	logPath := jobs.LogPath(req.Project, job.ID)
	for {
		select {
		case <-ctx.Done():
			_ = jobs.Stop(req.Project, job.ID)
			return SpawnResult{LogPath: logPath}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		cur, err := jobs.Get(req.Project, job.ID)
		if err != nil {
			return SpawnResult{LogPath: logPath}, err
		}
		switch cur.Status {
		case "done":
			return SpawnResult{Text: cur.Result, LogPath: logPath, ExitCode: cur.ExitCode}, nil
		case "failed", "stopped":
			return SpawnResult{Text: cur.Result, LogPath: logPath, ExitCode: cur.ExitCode}, nil
		}
	}
}
