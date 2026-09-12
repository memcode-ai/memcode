package taskrun

import (
	"context"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/task"
)

func work(project string, o Outcome, changed bool) projectWork {
	return projectWork{Project: project, Res: Result{
		Outcome: o, Changed: changed, ExecStatus: ExecCompleted, Summary: string(o),
	}}
}

// A parent run is never better than its worst project. "✓ succeeded — PR opened
// in repo A" while repo B could not be updated is the exact report this whole
// design exists to prevent.
func TestParentIsNeverBetterThanItsWorstProject(t *testing.T) {
	got := aggregate([]projectWork{
		work("/a", OutcomeSuccess, true),
		work("/b", OutcomeFailed, false),
	}, 2)
	if got.OKOutcome() {
		t.Fatalf("a failed project must not produce an OK parent: %+v", got)
	}
	if !strings.Contains(got.Summary, "need attention") {
		t.Errorf("the headline must name the trouble, got %q", got.Summary)
	}
}

// Stopping early is itself a finding. A coordinated run that gives up after the
// first bad project must not report on the projects it never reached as though
// they were fine.
func TestUnreachedProjectsMakeTheRunNeedAttention(t *testing.T) {
	got := aggregate([]projectWork{work("/a", OutcomeNoChange, false)}, 3)
	if got.Outcome != OutcomeNeedsAttention {
		t.Errorf("1 of 3 projects done should need attention, got %s", got.Outcome)
	}
	if !strings.Contains(got.Detail, "1 of 3") {
		t.Errorf("the detail must say how far it got:\n%s", got.Detail)
	}
}

func TestAllSucceedingIsSuccess(t *testing.T) {
	got := aggregate([]projectWork{
		work("/a", OutcomeSuccess, true),
		work("/b", OutcomeNoChange, false),
	}, 2)
	if got.Outcome != OutcomeSuccess {
		t.Errorf("success + no_change should be success, got %s", got.Outcome)
	}
	if !got.Changed {
		t.Error("a change in any project is a change in the run")
	}
}

// allOK is what gates coordinated publication, so it has to treat "we never got
// there" as not-ok rather than as an empty success.
func TestAllOKRequiresEveryProjectToHaveRun(t *testing.T) {
	ok := []projectWork{work("/a", OutcomeSuccess, true), work("/b", OutcomeSuccess, true)}
	if !allOK(ok, 2) {
		t.Error("two successes out of two should be ok")
	}
	if allOK(ok[:1], 2) {
		t.Error("one success out of two must NOT be ok — the other never ran")
	}
	if allOK([]projectWork{work("/a", OutcomeSuccess, true), work("/b", OutcomeBlocked, false)}, 2) {
		t.Error("a blocked project must not count as ok")
	}
}

// Targets is what the runner loops over, so a single-project task must produce
// exactly one target and a cross-project one must lead with its anchor.
func TestTargetsLeadWithTheAnchorAndDeduplicate(t *testing.T) {
	tk := task.Task{Project: "/repo/a"}
	if got := tk.Targets(); len(got) != 1 || got[0] != "/repo/a" {
		t.Fatalf("single project: got %v", got)
	}
	if tk.CrossProject() {
		t.Error("one project is not cross-project")
	}

	tk.Ownership.Projects = []string{"/repo/b", "/repo/a"}
	got := tk.Targets()
	if len(got) != 2 || got[0] != "/repo/a" || got[1] != "/repo/b" {
		t.Fatalf("anchor must come first and repeats must collapse: %v", got)
	}
	if !tk.CrossProject() {
		t.Error("two projects is cross-project")
	}
}

// The per-project account is what a human reads after the headline, so it has
// to name every project and say which publication rule was in force.
func TestPerProjectAccountNamesEveryProject(t *testing.T) {
	tk := task.Task{Ownership: task.Ownership{Coordination: task.CoordCoordinated}}
	got := perProject([]projectWork{work("/x/alpha", OutcomeSuccess, true), work("/x/beta", OutcomeFailed, false)}, tk)
	for _, want := range []string{"alpha", "beta", "all-or-nothing"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// The lease is the only thing standing between two unattended runs and the same
// checkout. A cross-project run that locked only its anchor would leave every
// other project it mutates unprotected — the same bug, one directory to the left.
func TestEveryProjectIsLeased(t *testing.T) {
	tk := task.Task{Autonomy: task.Autonomy{Level: task.LevelBranch},
		Ownership: task.Ownership{Projects: []string{"/repo/b"}}}
	got := LeaseKeys(tk, "/repo/a")
	if len(got) != 2 {
		t.Fatalf("both projects must be leased, got %v", got)
	}
	// Sorted, so two runs over the same set take their locks in one order and
	// cannot deadlock waiting on each other.
	if got[0] > got[1] {
		t.Errorf("lease keys must be in a stable order, got %v", got)
	}

	ro := task.Task{Autonomy: task.Autonomy{Level: task.LevelReadOnly},
		Ownership: task.Ownership{Projects: []string{"/repo/b"}}}
	if k := LeaseKeys(ro, "/repo/a"); len(k) != 0 {
		t.Errorf("read-only runs mutate nothing and must not lock anyone out, got %v", k)
	}

	custom := task.Task{Autonomy: task.Autonomy{Level: task.LevelBranch},
		Concurrency: task.Concurrency{Key: "deploys"},
		Ownership:   task.Ownership{Projects: []string{"/repo/b"}}}
	if k := LeaseKeys(custom, "/repo/a"); len(k) != 1 || k[0] != "custom:deploys" {
		t.Errorf("a custom key is one resource by definition, got %v", k)
	}
}

// An agent working in one checkout must be told which share of a cross-project
// goal is its own; otherwise it reads a goal phrased over two repositories,
// finds half of it missing, and either fails or goes looking outside its
// worktree.
func TestCrossProjectInstructionsScopeTheAgentToOneProject(t *testing.T) {
	tk := task.Task{Instructions: "Keep the model catalog current.",
		Ownership: task.Ownership{Projects: []string{"/repo/b"}, Responsibility: "keeping model support consistent"}}
	got := instructionsFor(tk, "/repo/a", []string{"/repo/a", "/repo/b"})
	for _, want := range []string{"/repo/a ONLY", "/repo/b", "keeping model support consistent"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	solo := instructionsFor(tk, "/repo/a", []string{"/repo/a"})
	if strings.Contains(solo, "ONLY") {
		t.Errorf("a single-project run must not be told about a scope it does not have:\n%s", solo)
	}
}

// PRE-PUBLICATION ATOMICITY. Coordinated means nothing becomes visible to
// anyone until the whole set has passed. The property that makes it achievable
// is that committing is separable from pushing — if the two were one call, repo
// A would already be public by the time repo B was found to be uncommittable.
func TestLocalAndRemotePublicationAreSeparable(t *testing.T) {
	// prepare and publish must be distinct methods on the runner. This looks
	// like a tautology; it is a guard. Merging them back together would silently
	// turn coordinated publication into a slogan, and nothing else would fail.
	var r *Runner
	_ = r.prepare
	_ = r.publish
}

// unpublished is the ONLY evidence that a coordinated publication actually
// held. If it is wrong, a partial push gets reported as all-or-nothing.
func TestUnpublishedNamesWhatDidNotLand(t *testing.T) {
	pushed := work("/x/alpha", OutcomeSuccess, true)
	pushed.Res.PRURL = "https://example.test/pr/1"
	stuck := work("/x/beta", OutcomeSuccess, true)
	branched := work("/x/gamma", OutcomeSuccess, true)
	branched.Res.CreatedBranch = true

	works := []projectWork{pushed, stuck, branched}
	got := unpublished(works, []int{0, 1, 2})
	if len(got) != 1 || got[0] != "beta" {
		t.Fatalf("only the project that published nothing should be named, got %v", got)
	}
	// A project that was never prepared is not "unpublished" — it was never
	// expected to publish, and naming it would report a partial failure that
	// did not happen.
	if n := unpublished(works, []int{0}); len(n) != 0 {
		t.Errorf("only prepared projects count, got %v", n)
	}
}

// THE BOUNDARY RULE. Drift inside an approved project is the whole point;
// growing into a new repository is new authority and must come from a person.
// The structural half of that guarantee is that the runner only ever visits the
// approved set — a discovered project has no path into Targets.
func TestApprovedProjectSetIsTheOnlyThingTheRunnerVisits(t *testing.T) {
	tk := task.Task{Project: "/repo/a", Ownership: task.Ownership{
		Projects:       []string{"/repo/b"},
		Responsibility: "keeping model support consistent across the product",
	}}
	got := tk.Targets()
	if len(got) != 2 {
		t.Fatalf("targets = %v, want exactly the approved pair", got)
	}
	for _, p := range got {
		if p == "/repo/c" {
			t.Fatal("a project nobody approved must never be a target")
		}
	}

	// And the agent is told where the edge is, with the exit that replaces
	// crossing it. Six months from now the work may have moved from B to C; the
	// run must stop and say so rather than enrolling C on its own reasoning.
	inst := instructionsFor(tk, "/repo/b", got)
	for _, want := range []string{
		"approved boundary is the projects listed above",
		"do not clone it",
		"NEEDS ATTENTION",
		"keeping model support consistent across the product",
	} {
		if !strings.Contains(inst, want) {
			t.Errorf("boundary contract missing %q:\n%s", want, inst)
		}
	}
}

// The boundary contract belongs to cross-project runs. A single-project task
// should not be lectured about a boundary it cannot cross.
func TestSingleProjectRunsGetNoBoundaryLecture(t *testing.T) {
	tk := task.Task{Project: "/repo/a", Instructions: "Do the thing."}
	if strings.Contains(instructionsFor(tk, "/repo/a", tk.Targets()), "approved boundary") {
		t.Error("a one-repo task has no boundary to state")
	}
}

// Recorded steps are an OPTIMISATION, never a verdict.
//
// They may end a run early in exactly one way: by doing the job and finding
// nothing to do. A step that FAILS means the world moved — a renamed tool, a
// changed command — and that is precisely when the agent is needed, so failure
// must escalate rather than report an outcome.
func TestFailingStepsEscalateRatherThanConclude(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	root := repo(t)
	tk := sample(t, "version: 1\nname: deps\ninstructions: Keep deps current.\n"+
		"execution:\n  steps:\n    - exit 3\n")

	var spawned bool
	r := runner(t, s, func(_ context.Context, req SpawnRequest) (SpawnResult, error) {
		spawned = true
		// The agent must be able to SEE what the steps did, or it has to guess
		// whether they ran.
		if !strings.Contains(req.Instructions, "the recorded mechanical steps ran first") {
			t.Error("the agent was not told what the steps did")
		}
		if !strings.Contains(req.Instructions, "exit 3") {
			t.Error("the failing step is missing from what the agent was given")
		}
		return SpawnResult{Text: "worked out the new command and upgraded"}, nil
	})
	run, err := r.Run(ctx, tk, root, TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if !spawned {
		t.Fatal("a failed step must hand over to the agent, not end the run")
	}
	if run.Outcome == OutcomeFailed {
		t.Errorf("the agent recovered; the run should not be failed: %s — %s", run.Outcome, run.Summary)
	}
}

// The payoff case: the mechanical part did the whole job, nothing changed, and
// no model was paid for the Monday.
func TestCleanStepsWithNoChangeSkipTheAgentEntirely(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	root := repo(t)
	tk := sample(t, "version: 1\nname: deps\ninstructions: Keep deps current.\n"+
		"execution:\n  steps:\n    - true\n")

	var spawned bool
	r := runner(t, s, func(context.Context, SpawnRequest) (SpawnResult, error) {
		spawned = true
		return SpawnResult{Text: "should not have run"}, nil
	})
	run, err := r.Run(ctx, tk, root, TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if spawned {
		t.Error("steps that succeeded and changed nothing must not cost a model call")
	}
	if run.Outcome != OutcomeNoChange {
		t.Errorf("outcome = %s, want no_change", run.Outcome)
	}
}
