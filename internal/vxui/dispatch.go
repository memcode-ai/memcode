package vxui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/jobs"
)

// dispatchSlash handles /dispatch <task>: spawns a hands-off background sub-agent
// via jobs.Spawn (the same detached-child primitive as `memcode run --background`).
// The sub-agent runs the full mutating agent loop with NO prompts or clarifying
// questions, serialized behind the repo writer lock. Fire-and-forget: the session
// keeps going and the footer tracks the live agent count.
func (s *appState) dispatchSlash(args string) {
	task := strings.TrimSpace(args)
	if task == "" {
		s.sysln("usage: /dispatch <task>")
		return
	}
	mode := string(permissions.ModeAuto) // a backgrounded agent can't answer prompts
	// Run the spawn in a goroutine: jobs.Spawn does cmd.Start + Process.Release
	// (fast, but touches the filesystem and execs — keep it off the UI thread).
	go func() {
		chrome := s.w.sess.BrowserEnabled()
		job, err := jobs.Spawn(s.w.sess.Root(), task, mode, "", chrome, false, "")
		s.rt.Dispatch(func() {
			if err != nil {
				s.sysln(fmt.Sprintf("couldn't dispatch: %v", err))
				return
			}
			s.sysln(fmt.Sprintf("◆ dispatch → started %s (pid %d) — %s", job.ID, job.PID, clipTask(task)))
			s.sysln(fmt.Sprintf("  running hands-off in %s mode. Follow with /agents or `memcode jobs logs %s`.", mode, job.ID))
			s.refreshFooter() // pick up the new count immediately
		})
	}()
}

// agentsSlash handles /agents: lists dispatched sub-agents (the detached agent jobs
// from /dispatch and `memcode run --background`), NOT the in-session shell jobs
// (those are /jobs). Runs jobs.List in a goroutine (a disk read) and prints a table.
//
// /agents stop <id> terminates a running agent (the safety valve for a runaway —
// signals the PID and records the job as stopped). /jobs kill <n> is a different
// system (in-session shells); /agents stop is for detached agents.
func (s *appState) agentsSlash(args string) {
	args = strings.TrimSpace(args)
	if strings.HasPrefix(args, "stop ") || args == "stop" {
		id := strings.TrimSpace(strings.TrimPrefix(args, "stop"))
		if id == "" {
			s.sysln("usage: /agents stop <id>")
			return
		}
		s.runAsync(func(context.Context) string {
			if err := jobs.Stop(s.w.sess.Root(), id); err != nil {
				return "couldn't stop agent: " + err.Error()
			}
			s.rt.Dispatch(func() { s.refreshFooter() }) // count may drop
			return "◆ stopped agent " + id
		})
		return
	}
	s.runAsync(func(context.Context) string {
		list, err := jobs.List(s.w.sess.Root())
		if err != nil {
			return "couldn't list agents: " + err.Error()
		}
		agents := filterAgents(list)
		if len(agents) == 0 {
			return "no dispatched agents."
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("Dispatched agents (%d):\n", len(agents)))
		for _, j := range agents {
			elapsed := time.Since(j.StartedAt).Round(time.Second)
			status := j.Status
			if j.Status == jobs.StatusDone {
				status = "✓ done"
			} else if j.Status == jobs.StatusFailed {
				status = fmt.Sprintf("✗ failed (exit %d)", j.ExitCode)
			} else if j.Status == jobs.StatusStopped {
				status = "✗ stopped"
			}
			b.WriteString(fmt.Sprintf("  %-10s %-8s pid %-6s %s  %s\n",
				j.ID, status, fmt.Sprintf("%d", j.PID), elapsed, clipTask(j.Task)))
		}
		b.WriteString("\n  follow logs with `memcode jobs logs <id>`")
		return strings.TrimRight(b.String(), "\n")
	})
}

// agentCount returns the number of currently-running dispatched agents (for the
// footer's "N agents" segment). Called from refreshFooter's goroutine.
func agentCount(root string) int {
	list, err := jobs.List(root)
	if err != nil {
		return 0
	}
	n := 0
	for _, j := range list {
		if j.Status == jobs.StatusRunning {
			n++
		}
	}
	return n
}

// agentReportBack is a finished report-back agent's output, handed back to the calling LLM as a
// new turn (the `agent{background:true}` contract — distinct from fire-and-forget dispatch).
type agentReportBack struct {
	ID, Task, Result string
}

// agentDoneNotifications detects agents that transitioned from running → a terminal status since
// the last check. It returns user-facing scrollback notification lines for each, AND the
// report-backs for any finished job that asked for one (so the caller can feed the result to the
// engine). The seen map (job id → last status) is read and updated in place.
func agentDoneNotifications(root string, seen map[string]string) (notes []string, backs []agentReportBack) {
	list, err := jobs.List(root)
	if err != nil {
		return nil, nil
	}
	for _, j := range list {
		prev, wasTracked := seen[j.ID]
		// Track every job we see; seed new ones as their current status (so an
		// already-finished job on launch doesn't notify — only transitions do).
		seen[j.ID] = j.Status
		if !wasTracked || prev == j.Status {
			continue
		}
		if prev != jobs.StatusRunning {
			continue // only notify on running → terminal
		}
		// running → done/failed/stopped: surface a notification.
		summary := agentSummary(root, j.ID)
		switch j.Status {
		case jobs.StatusDone:
			notes = append(notes, fmt.Sprintf("◆ agent %s finished — %s%s", j.ID, clipTask(j.Task), summaryLine(summary)))
		case jobs.StatusFailed:
			notes = append(notes, fmt.Sprintf("◆ agent %s failed (exit %d) — %s%s", j.ID, j.ExitCode, clipTask(j.Task), summaryLine(summary)))
		case jobs.StatusStopped:
			notes = append(notes, fmt.Sprintf("◆ agent %s stopped — %s. See /agents or `memcode jobs logs %s`.", j.ID, clipTask(j.Task), j.ID))
		}
		// Report-back agents (agent{background}) hand their RESULT back to the LLM on success.
		if j.ReportBack && j.Status == jobs.StatusDone {
			result := j.Result
			if strings.TrimSpace(result) == "" {
				result = summary // fall back to the log tail if the final text wasn't captured
			}
			backs = append(backs, agentReportBack{ID: j.ID, Task: j.Task, Result: result})
		}
	}
	return notes, backs
}

// agentSummary reads the tail of a job's log file for a brief "report back" summary.
// Returns the last ~15 lines, truncated to ~500 chars. Empty on any error.
func agentSummary(root, id string) string {
	data, err := os.ReadFile(jobs.LogPath(root, id))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	tail := lines
	if len(lines) > 15 {
		tail = lines[len(lines)-15:]
	}
	s := strings.Join(tail, "\n")
	if len(s) > 500 {
		s = "…" + s[len(s)-500:]
	}
	return strings.TrimSpace(s)
}

// summaryLine formats a log summary for a notification, or returns "" when empty.
func summaryLine(summary string) string {
	s := strings.TrimSpace(summary)
	if s == "" {
		return ". See /agents or `memcode jobs logs`."
	}
	return "\n  " + s
}

// filterAgents returns only agent jobs (not shell jobs). Currently all jobs from the
// jobs package are agent jobs (the in-session shell registry is a separate package),
// so this is a passthrough — but it's the seam to filter if the two ever merge.
func filterAgents(list []jobs.Job) []jobs.Job { return list }

// seedSeenAgents builds the initial seen-map from the current job list, so jobs that
// are ALREADY running/finished on launch don't trigger a spurious notification. Only
// running→terminal transitions notify — seeding at current status means the first tick
// sees no transition for pre-existing jobs.
func seedSeenAgents(root string) map[string]string {
	list, err := jobs.List(root)
	if err != nil {
		return map[string]string{}
	}
	m := make(map[string]string, len(list))
	for _, j := range list {
		m[j.ID] = j.Status
	}
	return m
}

// clipTask truncates a task description to one line (for tables/markers).
func clipTask(task string) string {
	task = strings.ReplaceAll(task, "\n", " ")
	if len([]rune(task)) > 80 {
		return string([]rune(task)[:77]) + "…"
	}
	return task
}
