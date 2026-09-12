package taskrun

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Verification is a RUNNER-OWNED phase, not extra prompt text.
//
// The agent is never asked whether the tests passed. It reports what it likes;
// the runner runs the commands, reads the exit codes, and decides. That
// separation is the whole reason this phase exists: an agent that believes it
// succeeded and an agent that wants to have succeeded produce identical prose,
// and for three runs straight the same denied write was narrated three
// different ways. Exit codes do not have a writing style.

// CheckResult is one verification command's outcome.
type CheckResult struct {
	Command  string
	ExitCode int
	Duration time.Duration
	// Output is the tail of combined stdout/stderr, kept small: enough to see
	// what failed, not enough to bury the run record.
	Output string
	// Err is set when the command could not be run at all, as distinct from
	// running and failing.
	Err string
}

// OK reports whether the command succeeded.
func (c CheckResult) OK() bool { return c.Err == "" && c.ExitCode == 0 }

// VerificationStatus is the verdict over all checks, kept separate from whether
// the agent's execution completed. A run whose agent finished cleanly and whose
// tests then failed is an execution success and a task failure, and collapsing
// those two into one enum is how that distinction gets lost.
type VerificationStatus string

const (
	// VerifyNone: the task declared no checks. Not a pass — an absence.
	VerifyNone VerificationStatus = "none"
	VerifyPass VerificationStatus = "passed"
	VerifyFail VerificationStatus = "failed"
	// VerifySkipped: the run never got far enough to verify.
	VerifySkipped VerificationStatus = "skipped"
)

// ExecutionStatus is whether the agent's half of the run completed.
type ExecutionStatus string

const (
	ExecCompleted   ExecutionStatus = "completed"
	ExecFailed      ExecutionStatus = "failed"
	ExecTimedOut    ExecutionStatus = "timed_out"
	ExecInterrupted ExecutionStatus = "interrupted"
	ExecBlocked     ExecutionStatus = "blocked"
)

// checkTimeout bounds one verification command so a hung test cannot hold a
// run, and its lease, open forever.
const checkTimeout = 15 * time.Minute

// maxCheckOutput is how much of a failing command's output is kept.
const maxCheckOutput = 4000

// Verify runs a task's checks in order, in dir. Every check runs even after one
// fails: "which of the four broke" is more useful than "the first one broke",
// and the cost is bounded by checkTimeout.
func Verify(ctx context.Context, dir string, commands []string, env ...string) ([]CheckResult, VerificationStatus) {
	if len(commands) == 0 {
		return nil, VerifyNone
	}
	results := make([]CheckResult, 0, len(commands))
	status := VerifyPass
	for _, c := range commands {
		r := runCheck(ctx, dir, c, env...)
		results = append(results, r)
		if !r.OK() {
			status = VerifyFail
		}
	}
	return results, status
}

func runCheck(ctx context.Context, dir, command string, env ...string) CheckResult {
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	// Run through a shell so a check reads the way a developer would type it.
	// This is the TASK AUTHOR's command from a file they wrote, not model
	// output, so it is trusted the way a Makefile is.
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()

	res := CheckResult{Command: command, Duration: time.Since(start), Output: tail(string(out), maxCheckOutput)}
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		res.ExitCode = -1
		res.Err = fmt.Sprintf("timed out after %s", checkTimeout)
	case err != nil:
		var ee *exec.ExitError
		if asExitError(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Err = err.Error()
		}
	}
	return res
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// tail keeps the END of output: a failing build's useful line is its last one.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// Summarize renders check results for a run record.
func Summarize(results []CheckResult) string {
	if len(results) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range results {
		mark := "ok  "
		if !r.OK() {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "%s %s (%s)", mark, r.Command, r.Duration.Round(time.Millisecond))
		if r.Err != "" {
			fmt.Fprintf(&b, " — %s", r.Err)
		} else if r.ExitCode != 0 {
			fmt.Fprintf(&b, " — exit %d", r.ExitCode)
		}
		b.WriteByte('\n')
	}
	// Only failures carry their output: a passing suite's log is noise in a
	// record someone is skimming for what went wrong.
	for _, r := range results {
		if !r.OK() && r.Output != "" {
			fmt.Fprintf(&b, "\n--- %s ---\n%s\n", r.Command, r.Output)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Decide derives the final outcome from FACTS, never from the agent's prose.
//
// The precedence is deliberate and one-directional: a blocked run cannot be
// talked up to failed, a failed verification cannot be talked up to success,
// and an agent's needs_attention signal can only ever make the verdict more
// cautious. The agent participates in this decision at exactly one point — it
// may raise needs_attention — and has no path to lower it.
func Decide(exec ExecutionStatus, verify VerificationStatus, changed bool, agentNeedsAttention bool) Outcome {
	switch exec {
	case ExecBlocked:
		return OutcomeBlocked
	case ExecInterrupted:
		return OutcomeInterrupted
	case ExecFailed, ExecTimedOut:
		return OutcomeFailed
	}
	// Execution completed. Verification is the next authority.
	if verify == VerifyFail {
		return OutcomeFailed
	}
	if agentNeedsAttention {
		return OutcomeNeedsAttention
	}
	if !changed {
		return OutcomeNoChange
	}
	return OutcomeSuccess
}
