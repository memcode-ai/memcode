package taskrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/task"
)

// Verification is RUNNER-owned: exit codes decide, not the agent's report.
func TestVerifyReadsExitCodes(t *testing.T) {
	dir := t.TempDir()
	checks, status := Verify(context.Background(), dir, []string{"true", "false", "true"})
	if status != VerifyFail {
		t.Errorf("status = %q, want failed", status)
	}
	if len(checks) != 3 {
		t.Fatalf("ran %d checks, want all 3 — knowing WHICH broke matters", len(checks))
	}
	if !checks[0].OK() || checks[1].OK() || !checks[2].OK() {
		t.Errorf("results = %+v, want ok/fail/ok", checks)
	}
	if checks[1].ExitCode == 0 {
		t.Error("a failing command must record a non-zero exit code")
	}
}

func TestVerifyNoneIsNotAPass(t *testing.T) {
	_, status := Verify(context.Background(), t.TempDir(), nil)
	if status != VerifyNone {
		t.Errorf("status = %q, want none — an absence of checks is not a passing suite", status)
	}
}

func TestVerifyRunsInTheGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, status := Verify(context.Background(), dir, []string{"test -f marker"})
	if status != VerifyPass {
		t.Errorf("status = %q — checks must run in the run's working directory", status)
	}
}

func TestSummarizeShowsOnlyFailingOutput(t *testing.T) {
	checks, _ := Verify(context.Background(), t.TempDir(),
		[]string{"true", "sh -c 'echo the-failure >&2; exit 3'"})
	got := Summarize(checks)

	// Every check is listed with its verdict...
	if !strings.Contains(got, "ok   true") || !strings.Contains(got, "FAIL") {
		t.Errorf("every check must appear with its verdict:\n%s", got)
	}
	if !strings.Contains(got, "exit 3") {
		t.Errorf("the exit code must be recorded:\n%s", got)
	}
	// ...but exactly ONE output block, belonging to the failure. A passing
	// suite's log is noise in a record someone is skimming for what broke.
	if n := strings.Count(got, "\n--- "); n != 1 {
		t.Errorf("got %d output blocks, want only the failing one:\n%s", n, got)
	}
	if !strings.Contains(got, "the-failure") {
		t.Errorf("the failing check's output must be kept:\n%s", got)
	}
}

// THE outcome model. Each row is a claim about what decides a verdict, and the
// direction of authority is one-way: facts can lower an outcome, the agent can
// only ever raise caution.
func TestDecideIsDeterministic(t *testing.T) {
	cases := []struct {
		name    string
		exec    ExecutionStatus
		verify  VerificationStatus
		changed bool
		agent   bool
		want    Outcome
	}{
		{"blocked beats everything", ExecBlocked, VerifyPass, true, false, OutcomeBlocked},
		{"interrupted beats everything", ExecInterrupted, VerifyPass, true, false, OutcomeInterrupted},
		{"exec failure", ExecFailed, VerifyNone, false, false, OutcomeFailed},
		{"timeout", ExecTimedOut, VerifyNone, false, false, OutcomeFailed},

		// The heart of it: a clean agent run whose tests failed is a FAILURE.
		{"clean run, failed tests", ExecCompleted, VerifyFail, true, false, OutcomeFailed},
		// ...and the agent cannot talk it back up.
		{"agent cannot override failed tests", ExecCompleted, VerifyFail, true, true, OutcomeFailed},

		{"changed and verified", ExecCompleted, VerifyPass, true, false, OutcomeSuccess},
		{"changed, no checks declared", ExecCompleted, VerifyNone, true, false, OutcomeSuccess},
		{"nothing changed", ExecCompleted, VerifyPass, false, false, OutcomeNoChange},
		// The agent's one input: raising caution on a run that otherwise passed.
		{"agent raises attention", ExecCompleted, VerifyPass, true, true, OutcomeNeedsAttention},
		{"agent raises attention with no change", ExecCompleted, VerifyPass, false, true, OutcomeNeedsAttention},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.exec, c.verify, c.changed, c.agent); got != c.want {
				t.Errorf("Decide(%s,%s,changed=%v,agent=%v) = %q, want %q",
					c.exec, c.verify, c.changed, c.agent, got, c.want)
			}
		})
	}
}

// There is no combination in which an agent's claim produces success from a
// failed or blocked run. Exhaustive rather than sampled, because this is the
// property the whole milestone exists to guarantee.
func TestAgentCanNeverNarrateItselfIntoSuccess(t *testing.T) {
	for _, exec := range []ExecutionStatus{ExecBlocked, ExecInterrupted, ExecFailed, ExecTimedOut} {
		for _, verify := range []VerificationStatus{VerifyNone, VerifyPass, VerifyFail, VerifySkipped} {
			for _, changed := range []bool{true, false} {
				if got := Decide(exec, verify, changed, false); got == OutcomeSuccess || got == OutcomeNoChange {
					t.Errorf("exec %s must never produce %q", exec, got)
				}
			}
		}
	}
	for _, changed := range []bool{true, false} {
		if got := Decide(ExecCompleted, VerifyFail, changed, true); got != OutcomeFailed {
			t.Errorf("failed verification must stay failed, got %q", got)
		}
	}
}

// An end-to-end run whose verification fails must be recorded as failed, keep
// its worktree for inspection, and say why.
func TestFailedVerificationKeepsTheWorktree(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	root := repo(t)
	tk := sample(t, `version: 1
name: breaks-tests
instructions: change something
verify:
  commands:
    - "false"
`)
	run, err := runner(t, s, changing("I changed things and everything is fine.")).
		Run(ctx, tk, root, TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != OutcomeFailed {
		t.Errorf("outcome = %q, want failed — the agent's optimism does not decide", run.Outcome)
	}
	if run.ExecStatus != ExecCompleted {
		t.Errorf("exec status = %q, want completed — the AGENT finished fine", run.ExecStatus)
	}
	if run.VerifyStatus != VerifyFail {
		t.Errorf("verify status = %q, want failed", run.VerifyStatus)
	}
	if run.Worktree == "" {
		t.Fatal("a failed run must keep its worktree — that checkout is the evidence")
	}
	if _, err := os.Stat(run.Worktree); err != nil {
		t.Errorf("the retained worktree must still exist: %v", err)
	}
	if !strings.Contains(run.Detail, run.Worktree) {
		t.Errorf("the run must record where to look, got %q", run.Detail)
	}
	if run.BaseRev == "" {
		t.Error("base revision must be recorded")
	}
}

// A run with nothing to lose cleans up after itself. Retention is decided by
// whether the work is recoverable elsewhere, not by whether the outcome reads
// nicely — see TestLocalOnlyRepoCommitsAndKeepsTheWork for the other side.
func TestNoChangeRunCleansItsWorktree(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	tk := sample(t, `version: 1
name: tidy
instructions: look around
verify:
  commands:
    - "true"
`)
	run, err := runner(t, s, ok("nothing to do here")).Run(ctx, tk, repo(t), TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != OutcomeNoChange {
		t.Fatalf("outcome = %q, want no_change", run.Outcome)
	}
	if run.Worktree != "" {
		t.Errorf("a run that changed nothing should clean its worktree, got %q", run.Worktree)
	}
	if run.BaseRev == "" || run.Branch == "" {
		t.Error("base revision and branch must still be recorded after cleanup")
	}
}

// The base revision is resolved at EXECUTION time. A Monday occurrence
// recovered on Wednesday builds on Wednesday's HEAD, and the record says so
// rather than implying the repository was frozen when the task was due.
func TestBaseRevisionIsResolvedAtExecutionTime(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	root := repo(t)

	head := func() string {
		out, err := gitOut(t, root, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := head()

	// The repository moves on before the task runs.
	if err := os.WriteFile(filepath.Join(root, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "later.txt")
	gitRun(t, root, "commit", "-qm", "moved on")
	after := head()
	if before == after {
		t.Fatal("precondition: HEAD should have moved")
	}

	tk := sample(t, "")
	run, err := runner(t, s, ok("looked around")).Run(ctx, tk, root, TriggerManual, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.BaseRev != after {
		t.Errorf("base = %s, want HEAD at execution time (%s), not when the task was due", run.BaseRev, after)
	}
	if run.ResultRev != run.BaseRev {
		t.Errorf("a run that committed nothing has result == base, got %s vs %s", run.ResultRev, run.BaseRev)
	}
	if run.Changed {
		t.Error("a run that touched nothing must not report a change")
	}
}

// Capability projection is authoritative: the tier decides what the child gets.
func TestCapabilityProjection(t *testing.T) {
	ro, err := sample(t, "version: 1\nname: a\ninstructions: x\nautonomy:\n  level: read_only\n").Capability()
	if err != nil {
		t.Fatal(err)
	}
	br, err := sample(t, "").Capability()
	if err != nil {
		t.Fatal(err)
	}

	// read_only keeps its shell — a task that cannot build or test is useless —
	// but loses everything that could outlive the run.
	if contains(ro.DenyTools, "shell") {
		t.Error("read_only must keep the shell")
	}
	for _, want := range []string{"git push", "git commit", "gh"} {
		if !contains(ro.DenyCommands, want) {
			t.Errorf("read_only must deny %q", want)
		}
	}
	// branch may prepare a change...
	for _, unwanted := range []string{"git push", "git commit", "git branch"} {
		if contains(br.DenyCommands, unwanted) {
			t.Errorf("the branch tier must be allowed to %q", unwanted)
		}
	}
	// ...but never make one authoritative, at any tier.
	for _, tier := range []task.Capability{ro, br} {
		for _, want := range []string{"git push --force", "git merge", "git reset --hard", "gh pr merge"} {
			if !contains(tier.DenyCommands, want) {
				t.Errorf("%q must be denied at every tier, got %v", want, tier.DenyCommands)
			}
		}
	}
	if !ro.ProtectProject || !br.ProtectProject {
		t.Error("no autonomous run may execute in the user's checkout")
	}
}

func gitOut(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	out, err := execGit(dir, args...)
	return strings.TrimSpace(out), err
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := execGit(dir, args...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}

// execGit is a tiny helper so tests can move the repository under a run.
func execGit(dir string, args ...string) (string, error) {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := c.CombinedOutput()
	return string(out), err
}

// The real spawn path is the one part of a run an injected fake cannot cover,
// and a field missing from the spec is invisible until it matters. WorkDir went
// unset for a while: the child ran in the user's own checkout, the agent's file
// writes escaped the worktree entirely, and every test still passed.
func TestJobSpecCarriesTheIsolation(t *testing.T) {
	req := SpawnRequest{
		RunID:        "run_x",
		Project:      "/repo",
		WorkDir:      "/repo/.memcode/worktrees/auto-x",
		Instructions: "do the thing",
		DenyTools:    []string{"edit_file"},
		DenyCommands: []string{"git push"},
	}
	spec := jobSpec(req, req.WorkDir)

	if spec.WorkDir != req.WorkDir {
		t.Errorf("WorkDir = %q, want the worktree — without it the child runs in the user's checkout", spec.WorkDir)
	}
	if spec.Root != req.Project {
		t.Errorf("Root = %q, want the project, so the job log outlives a cleaned worktree", spec.Root)
	}
	if spec.Root == spec.WorkDir {
		t.Error("bookkeeping root and working directory must differ for an isolated run")
	}
	// The capability ceiling must reach the child, not merely be computed.
	if len(spec.ToolPolicy.Disabled) == 0 || spec.ToolPolicy.Disabled[0] != "edit_file" {
		t.Errorf("tool ceiling did not reach the spec: %+v", spec.ToolPolicy)
	}
	if len(spec.DenyCommands) == 0 || spec.DenyCommands[0] != "git push" {
		t.Errorf("command ceiling did not reach the spec: %v", spec.DenyCommands)
	}
	if spec.RunID != "run_x" {
		t.Errorf("RunID = %q, want the run's identity on the child", spec.RunID)
	}
}

// With no isolation, the child runs where the bookkeeping lives.
func TestJobSpecDefaultsWorkDirToProject(t *testing.T) {
	spec := jobSpec(SpawnRequest{Project: "/repo"}, "/repo")
	if spec.WorkDir != "/repo" {
		t.Errorf("WorkDir = %q, want the project", spec.WorkDir)
	}
}

// Every autonomous run is handed the drift contract, not just the task's own
// words. A scheduled task runs against a repository that has moved, and the
// failure it must avoid is not a crash — it is confidently doing the wrong
// thing because it assumed a layout that no longer exists.
func TestRunsReceiveTheDriftContract(t *testing.T) {
	tk := sample(t, `version: 1
name: drifty
instructions: Keep the provider catalog current.
`)
	tk.Execution.KnownGood = "(worked 2026-09-12) edited catalog/models.json directly"

	got := instructionsFor(tk, "/repo", []string{"/repo"})
	for _, want := range []string{
		"running unattended",
		"CURRENT state rather than assuming",
		"do not trust remembered paths",
		"evidence that it worked once, not as instructions",
		"Keep the provider catalog current.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the composed instructions must contain %q:\n%s", want, got)
		}
	}
	// The known-good approach is included, and labelled so it cannot be read as
	// authority.
	if !strings.Contains(got, "catalog/models.json") {
		t.Error("a recorded approach should be passed along")
	}
	if !strings.Contains(got, "Evidence, not authority") {
		t.Error("a recorded approach must be labelled as a hint")
	}
	// The goal must come after the contract, so the contract frames it.
	if strings.Index(got, "running unattended") > strings.Index(got, "Keep the provider catalog") {
		t.Error("the contract must precede the task's own instructions")
	}
}

// A task with no recorded approach gets the contract and nothing invented.
func TestDriftContractWithoutKnownGood(t *testing.T) {
	tk := sample(t, "version: 1\nname: plain\ninstructions: Do the thing.\n")
	got := instructionsFor(tk, "/repo", []string{"/repo"})
	if strings.Contains(got, "worked when this task was created") {
		t.Error("no recorded approach should mean no such section")
	}
	if !strings.Contains(got, "Do the thing.") {
		t.Error("the goal must survive")
	}
}
