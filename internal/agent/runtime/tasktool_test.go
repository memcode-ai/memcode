package runtime

import (
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/task"
)

func sampleTask() task.Task {
	return task.Task{
		Name:         "dependency-updates",
		Description:  "Keep dependencies current",
		Instructions: "Work out which modules have newer stable releases and upgrade them.",
		Triggers:     []task.Trigger{{Every: "168h"}},
		Autonomy:     task.Autonomy{Level: task.LevelBranch},
		Git:          task.Git{PullRequest: task.PRWhenChanges},
		Verify:       task.Verify{Commands: []string{"go test ./..."}},
	}
}

// The approval surface is the automation CONTRACT, not the file that backs it.
// Someone deciding whether to let this run while they sleep is judging
// behaviour and blast radius; if they have to read YAML vocabulary to do that,
// the report has failed and the wrong thing is being authorised.
func TestReportIsAContractNotAConfigDump(t *testing.T) {
	got := taskReport(sampleTask(), taskToolInput{
		Does:            []string{"upgrades dependencies that have newer stable releases"},
		SuccessCriteria: "the repo builds and the test suite passes",
		SideEffects:     []string{"opens a pull request"},
	}, validation{ok: true, how: "tried against this repo just now", detail: "success — upgraded 3 modules"})

	for _, want := range []string{
		"What it will do", "How it knows it worked", "What it may change", "Validated now",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing the %q section:\n%s", want, got)
		}
	}
	// Vocabulary from the representation underneath. Each of these is a real
	// field name or enum value in the YAML, and none of them belongs in front
	// of a user being asked to authorise an automation.
	for _, leak := range []string{
		"when_changes", "yaml", "autonomy", "grants", "worktree:", "trigger", "cron",
		"168h", "occurrence", "PRWhenChanges",
	} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(leak)) {
			t.Errorf("report leaks the representation (%q):\n%s", leak, got)
		}
	}
}

// A task cannot demonstrate that its strategy works today and still be
// presented as validated. The failure has to be in front of the user BEFORE
// they approve it, because that is the entire reason validation runs first.
func TestReportSaysSoWhenValidationFailed(t *testing.T) {
	got := taskReport(sampleTask(), taskToolInput{}, validation{
		how: "tried against this repo just now", detail: "failed — go test ./... exited 1",
	})
	if !strings.Contains(got, "did NOT work") {
		t.Errorf("a failed trial must be stated plainly:\n%s", got)
	}
	if !strings.Contains(got, "fail the same way unattended") {
		t.Errorf("a failed trial must say what creating it anyway means:\n%s", got)
	}
}

// Work already done successfully in the conversation is the validation. Running
// it again to prove a proven point costs the user time and money, and on a task
// with side effects it does the work twice.
func TestJustCompletedWorkCountsAsValidation(t *testing.T) {
	got := taskReport(sampleTask(), taskToolInput{}, validation{
		ok: true, how: "the work just done in this session", detail: "upgraded 6 modules; tests pass",
	})
	if !strings.Contains(got, "the work just done in this session") {
		t.Errorf("evidence from this session must be attributed as such:\n%s", got)
	}
	if strings.Contains(got, "did NOT work") {
		t.Errorf("successful prior work must not read as a failure:\n%s", got)
	}
}

// The contract always states the two promises the user cannot verify for
// themselves at approval time: that a run will not disturb their working tree,
// and that it re-derives state rather than replaying today's steps. A terse
// tool call must not be able to drop them.
func TestContractAlwaysStatesIsolationAndDrift(t *testing.T) {
	got := taskReport(sampleTask(), taskToolInput{}, validation{ok: true, how: "x", detail: "y"})
	if !strings.Contains(got, "never in your working tree") {
		t.Errorf("isolation promise missing:\n%s", got)
	}
	if !strings.Contains(got, "re-checks the current state each run") {
		t.Errorf("drift promise missing:\n%s", got)
	}
}

// read_only is the one authority claim in the report that would be actively
// dangerous to state wrongly.
func TestReadOnlyIsStatedAsSuch(t *testing.T) {
	tk := sampleTask()
	tk.Autonomy.Level = task.LevelReadOnly
	tk.Git.PullRequest = task.PRNever
	got := taskReport(tk, taskToolInput{}, validation{ok: true, how: "x", detail: "y"})
	if !strings.Contains(got, "cannot modify files") {
		t.Errorf("a read-only task must say so:\n%s", got)
	}
	if !strings.Contains(got, "nothing is pushed or published") {
		t.Errorf("a task that publishes nothing must say so:\n%s", got)
	}
}

// Storage follows OWNERSHIP. A responsibility spanning two repos, written into
// one of them, gives that repo an ownership it does not have: clone it alone
// and the task claims to maintain projects that are not there; clone the other
// and the concern has vanished.
func TestCrossProjectTasksAreStoredGlobally(t *testing.T) {
	tk := sampleTask()
	tk.Project = "/repo/a"
	if got := storageScope(tk, "/repo/a"); got != task.ScopeProject {
		t.Errorf("a single-repo responsibility belongs to that repo, got %s", got)
	}
	tk.Ownership.Projects = []string{"/repo/b"}
	if got := storageScope(tk, "/repo/a"); got != task.ScopeGlobal {
		t.Errorf("a cross-cutting responsibility is owned by no single repo, got %s", got)
	}
	if got := storageScope(sampleTask(), ""); got != task.ScopeGlobal {
		t.Errorf("outside a repo there is nowhere else to put it, got %s", got)
	}
}

// When the automation is cross-cutting the user must see exactly which
// checkouts they are handing over, before they hand them over.
func TestReportStatesTheScopeWhenCrossCutting(t *testing.T) {
	tk := sampleTask()
	tk.Project = "/repo/memcode"
	tk.Ownership = task.Ownership{
		Projects:     []string{"/repo/memcode-www"},
		Coordination: task.CoordCoordinated,
		Discover:     "the repos that implement the product",
	}
	got := taskReport(tk, taskToolInput{}, validation{ok: true, how: "x", detail: "y"})
	for _, want := range []string{
		"Scope", "/repo/memcode", "/repo/memcode-www",
		"all of them together, or none of them",
		"no one repo owns this",
		"re-confirms which projects are involved",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}

	// And says nothing about scope when there is only one project, so an
	// ordinary task's approval stays as short as it was.
	if strings.Contains(taskReport(sampleTask(), taskToolInput{}, validation{ok: true, how: "x", detail: "y"}), "Scope") {
		t.Error("a single-project automation should not grow a scope section")
	}
}
