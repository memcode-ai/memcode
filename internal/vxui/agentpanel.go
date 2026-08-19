package vxui

import (
	"fmt"
	"time"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"
	"github.com/memcode-ai/memcode/internal/jobs"
)

// The background-agents live region: a launch tree printed into the transcript
// when detached agents appear, plus a panel below the status row with one line
// per running agent (activity · elapsed · tokens), fed by the child processes'
// meta.json heartbeats. Visibility only — managing agents stays with /agents
// and `memcode jobs`; completion notices stay with refreshFooter's 5s loop.

// startAgentTicker polls jobs.List at 1Hz (off the UI thread, like
// refreshFooter) and marshals one Dispatch per tick: announce newly-spawned
// agents in the transcript, refresh the panel rows, and advance the spinner
// while agents run and no turn owns it. Zero Dispatch traffic when idle with
// no agents. Detection in one place covers every spawn path — the agent tool,
// the dispatch tool, /dispatch, `memcode run --background` from another
// terminal, and the gateway.
func (s *appState) startAgentTicker() {
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.w.ctx.Done():
				return
			case <-t.C:
				root := s.w.sess.Root()
				list, err := jobs.List(root)
				if err != nil {
					continue
				}
				var running []jobs.Job
				for _, j := range list {
					if j.Status == jobs.StatusRunning {
						running = append(running, j)
					}
				}
				s.rt.Dispatch(func() {
					// First pass seeds the known-set from ALL jobs and announces
					// nothing — agents already running when the TUI opened aren't
					// "launched" news (same idea as seedSeenAgents).
					if s.agentKnown == nil {
						s.agentKnown = make(map[string]bool, len(list))
						for _, j := range list {
							s.agentKnown[j.ID] = true
						}
						s.SetState(func() { s.agentJobs = running })
						return
					}
					var fresh []jobs.Job
					for _, j := range running {
						if !s.agentKnown[j.ID] {
							s.agentKnown[j.ID] = true
							fresh = append(fresh, j)
						}
					}
					for _, line := range launchBlock(fresh) {
						s.sysln(line)
					}
					if len(running) == 0 && len(s.agentJobs) == 0 {
						return // nothing to render, nothing to clear — skip the frame
					}
					s.SetState(func() {
						s.agentJobs = running
						// 1Hz spinner/elapsed animation; the turn spinner owns
						// s.spin while busy, so stand down (no double-advance).
						if !s.busy() && len(running) > 0 {
							s.spin++
						}
					})
				})
			}
		}
	}()
}

// launchBlock renders the transcript announcement for newly-spawned agents.
func launchBlock(fresh []jobs.Job) []string {
	if len(fresh) == 0 {
		return nil
	}
	noun := "background agents"
	if len(fresh) == 1 {
		noun = "background agent"
	}
	out := []string{fmt.Sprintf("⏺ %d %s launched (/agents to manage)", len(fresh), noun)}
	for i, j := range fresh {
		branch := "├"
		if i == len(fresh)-1 {
			branch = "└"
		}
		out = append(out, "   "+branch+" "+clipTask(j.Task))
	}
	return out
}

// agentPanel returns the live rows for running background agents — nil when
// there are none, so the panel vanishes (the todoPanel contract).
func (s *appState) agentPanel() []ui.Widget {
	if len(s.agentJobs) == 0 {
		return nil
	}
	now := time.Now()
	glyph := string(spinFrames[s.spin%len(spinFrames)])
	rows := make([]ui.Widget, 0, len(s.agentJobs))
	for _, j := range s.agentJobs {
		rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
			{Text: "  " + glyph + " ", Style: s.sty.brand},
			{Text: clipTask(j.Task), Style: s.sty.emph},
			{Text: formatAgentRow(j, now), Style: s.sty.muted},
		}, SoftWrap: true, MaxLines: 2})
	}
	return rows
}

// formatAgentRow renders the detail tail of one agent's panel line.
func formatAgentRow(j jobs.Job, now time.Time) string {
	activity := j.Activity
	if activity == "" {
		activity = "starting…"
	}
	out := " · " + activity + " · " + elapsedShort(now.Sub(j.StartedAt))
	if j.TokensOut > 0 {
		out += " · ↓" + fmtTokens(int(j.TokensOut)) + " tokens"
	}
	return out
}

// elapsedShort renders a duration as 42s / 1m32s / 1h04m.
func elapsedShort(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
