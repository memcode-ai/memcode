package runtime

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/jobs"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
)

// The job registry uses immutable internal IDs (stable for logs/audit). But users
// think in ACTIVE SHELLS, not historical attempts: "I started one server → /kill 1",
// even if an earlier attempt failed and never ran. So everything user- and agent-
// facing (/jobs, /tail, /kill, the footer, the start marker) speaks in SHELL SLOTS:
// the running jobs numbered 1..N by start order. A failed/finished job holds no slot,
// so a quick failed start never steals slot 1 from the real running shell.

// RunningJobs is the count of live shells — the footer's "N shells".
func (s *Session) RunningJobs() int { return s.bgJobs.Running() }

// ShellReportBack is a promoted foreground command that has finished and owes its result
// back to the model (redacted, ready to inject as a new turn). Identified by command, not
// shell slot — a finished job holds no slot.
type ShellReportBack struct {
	Command string
	Exit    int
	Failed  bool
	Output  string
}

// DrainShellReportBacks returns the promoted commands that finished since the last call,
// each once, with their result redacted for hand-back to the model. Polled by the UI.
func (s *Session) DrainShellReportBacks() []ShellReportBack {
	var out []ShellReportBack
	for _, r := range s.bgJobs.DrainReports() {
		out = append(out, ShellReportBack{
			Command: s.redactor.Redact(r.Command),
			Exit:    r.Exit,
			Failed:  r.Status == jobs.Failed,
			Output:  s.redactor.Redact(r.Output),
		})
	}
	return out
}

// ReportPrompt renders a finished background shell (started or promoted) as the
// turn text handed to the model.
func (b ShellReportBack) ReportPrompt() string {
	status := fmt.Sprintf("exit %d", b.Exit)
	if b.Failed {
		status = fmt.Sprintf("FAILED (exit %d)", b.Exit)
	}
	out := strings.TrimSpace(b.Output)
	if out == "" {
		out = "(no output)"
	}
	return fmt.Sprintf("[background shell finished] a command running in the background has completed.\ncommand: %s\nresult: %s\n\nAct on this result: report the outcome to the user (and continue any work that was waiting on it).\n\nOutput:\n%s", b.Command, status, truncate(out, maxToolOutput))
}

// KillAllJobs reaps every running job (session end — nothing orphans).
func (s *Session) KillAllJobs() { s.bgJobs.KillAll() }

// runningOrdered returns the running jobs in start order (internal id asc); the
// 1-based position is the user-facing shell slot.
func (s *Session) runningOrdered() []jobs.View {
	var r []jobs.View
	for _, v := range s.bgJobs.List() {
		if v.Status == jobs.Running {
			r = append(r, v)
		}
	}
	sort.Slice(r, func(i, j int) bool { return r[i].ID < r[j].ID })
	return r
}

// jobForSlot resolves a 1-based shell slot to its running job.
func (s *Session) jobForSlot(slot int) (jobs.View, bool) {
	r := s.runningOrdered()
	if slot >= 1 && slot <= len(r) {
		return r[slot-1], true
	}
	return jobs.View{}, false
}

// slotForID returns the shell slot of a running job, or 0 if it isn't running.
func (s *Session) slotForID(id int) int {
	for i, v := range s.runningOrdered() {
		if v.ID == id {
			return i + 1
		}
	}
	return 0
}

func shellList(running []jobs.View) string {
	var b strings.Builder
	for i, v := range running {
		fmt.Fprintf(&b, "  shell %d  %s\n", i+1, clip(v.Command, 50))
	}
	return strings.TrimRight(b.String(), "\n")
}

// runShellBackground starts a `$ … &` command as a background shell: it does NOT
// block the turn, so dev servers / watchers / log tails live here. Gated like any $
// command; the job runs under the long-lived session ctx.
func (s *Session) runShellBackground(ctx context.Context, command string) {
	prompt := shellPromptStyle().Render("$") + " " + command + " &"
	risk, catastrophic := permissions.ClassifyBash(command)
	ok, run, reason := s.gateCommand(ctx, risk, catastrophic, command, "")
	s.allowPending = "" // user-typed lane: the user IS the authorization — no provenance sub-line (see runShell)
	if !ok {
		s.emitRaw(prompt + "\n" + delStyle().Render("✖ "+orEmpty(reason, "denied")))
		return
	}
	v, err := jobs.Start(s.bgJobs, s.bgCtx, run, s.root)
	if err != nil {
		s.emitRaw(prompt + "\n" + delStyle().Render("✖ "+err.Error()))
		return
	}
	slot := s.slotForID(v.ID)
	s.logShell(run+" &", fmt.Sprintf("(shell %d)", slot), 0)
	s.emitRaw(prompt + "\n" +
		addStyle().Render(fmt.Sprintf("▶ shell %d running", slot)) +
		metaStyle.Render(fmt.Sprintf("   ·   /tail %d · /kill %d", slot, slot)))
}

// JobsRender lists the running shells (by slot) for /jobs. Finished/failed attempts
// aren't shown — they live in the session log, not the user's working set.
func (s *Session) JobsRender() string {
	running := s.runningOrdered()
	if len(running) == 0 {
		return "no running shells."
	}
	var b strings.Builder
	for i, v := range running {
		fmt.Fprintf(&b, "shell %d  %s  %s\n", i+1, time.Since(v.Started).Round(time.Second), clip(v.Command, 50))
	}
	b.WriteString("\n/tail <n> · /kill <n>")
	return s.redactor.Redact(strings.TrimRight(b.String(), "\n"))
}

// KillJobArg powers /kill (and the agent's jobs-kill). The arg is a SHELL SLOT: empty
// stops the lone running shell (lists when several); a slot that isn't running shows
// what IS running instead of a dead-end.
func (s *Session) KillJobArg(arg string) string {
	arg = strings.TrimSpace(arg)
	running := s.runningOrdered()
	if arg == "" {
		switch len(running) {
		case 0:
			return "no running shells."
		case 1:
			return s.stopShell(1, running[0])
		default:
			return "several shells running — say /kill <n>:\n" + shellList(running)
		}
	}
	slot, err := strconv.Atoi(arg)
	if err != nil {
		return "usage: /kill <shell-number>  (or /kill alone for the only running shell)"
	}
	v, ok := s.jobForSlot(slot)
	if !ok {
		if len(running) == 0 {
			return fmt.Sprintf("no shell %d — nothing is running.", slot)
		}
		return fmt.Sprintf("no running shell %d. running:\n%s", slot, shellList(running))
	}
	return s.stopShell(slot, v)
}

func (s *Session) stopShell(slot int, v jobs.View) string {
	if s.bgJobs.Kill(v.ID) {
		return fmt.Sprintf("stopped shell %d — %s", slot, clip(v.Command, 50))
	}
	return fmt.Sprintf("shell %d already stopped.", slot)
}

// TailJobArg powers /tail: the arg is a shell slot; empty tails the lone running shell.
func (s *Session) TailJobArg(arg string) string {
	arg = strings.TrimSpace(arg)
	running := s.runningOrdered()
	var slot int
	switch {
	case arg == "" && len(running) == 0:
		return "no running shells to tail."
	case arg == "" && len(running) == 1:
		slot = 1
	case arg == "":
		return "several shells running — say /tail <n>:\n" + shellList(running)
	default:
		n, err := strconv.Atoi(arg)
		if err != nil {
			return "usage: /tail <shell-number>"
		}
		slot = n
	}
	v, ok := s.jobForSlot(slot)
	if !ok {
		return fmt.Sprintf("no running shell %d.", slot)
	}
	out, _ := s.bgJobs.Tail(v.ID, 20)
	if strings.TrimSpace(out) == "" {
		out = "(no output yet)"
	}
	return s.redactor.Redact(fmt.Sprintf("shell %d — %s:\n%s", slot, clip(v.Command, 40), out))
}

// introspectJobs is the agent's STRUCTURED access to shells (the memcode "jobs"
// command): list (default) | kill <slot> | tail <slot>. The agent must use this to
// stop or inspect a shell it started — a shell `kill %1` runs in a throwaway sh and
// does NOT touch the managed job (and leaves the registry/footer out of sync).
func (s *Session) introspectJobs(target string) (string, bool) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(target)))
	if len(fields) == 0 || fields[0] == "list" || fields[0] == "jobs" {
		return s.JobsRender(), false
	}
	arg := ""
	if len(fields) > 1 {
		arg = fields[1]
	}
	switch fields[0] {
	case "kill", "stop":
		return s.KillJobArg(arg), false
	case "tail", "logs":
		return s.TailJobArg(arg), false
	default:
		return s.JobsRender(), false
	}
}
