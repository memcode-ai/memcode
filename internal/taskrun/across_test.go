package taskrun

import (
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
		Ownership: task.Ownership{Projects: []string{"/repo/b"}, Discover: "the repos that build the product"}}
	got := instructionsFor(tk, "/repo/a", []string{"/repo/a", "/repo/b"})
	for _, want := range []string{"/repo/a ONLY", "/repo/b", "the repos that build the product"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	solo := instructionsFor(tk, "/repo/a", []string{"/repo/a"})
	if strings.Contains(solo, "ONLY") {
		t.Errorf("a single-project run must not be told about a scope it does not have:\n%s", solo)
	}
}
