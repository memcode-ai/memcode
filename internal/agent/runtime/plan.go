package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/plans"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/wire"
)

// recallPlanTool retrieves a previously presented plan so the user can pick one up in natural
// language ("resume the last plan"). It merges two recovery paths — this project's session logs
// (the canonical record, full text) and the user-level ~/.memcode/plans store (cross-project) — so
// a plan is found even when the store is empty (a prior session, a wiped store, a fresh machine).
// Empty slug → the most recent plan in full plus a list of others; a slug → that specific plan.
func (s *Session) recallPlanTool(_ context.Context, input json.RawMessage) toolResult {
	var in tools.RecallPlanInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	slug := strings.TrimSpace(in.Slug)
	refs := s.recallablePlans()
	if len(refs) == 0 {
		s.toolLine(true, "RecallPlan", "", "none found", false)
		return textResult("No plans found — memcode saves a plan to ~/.memcode/plans and tags it in this project's session log each time one is presented, and none exist yet.")
	}
	target := refs[0] // default: most recent
	if slug != "" {
		found := false
		for _, r := range refs {
			if r.slug == slug {
				target, found = r, true
				break
			}
		}
		if !found {
			s.toolLine(true, "RecallPlan", slug, "not found", true)
			return errResult("No saved plan named " + slug + ". Available plans: " + recalledList(refs, 12) + ".")
		}
	}
	text, err := target.load()
	if err != nil || strings.TrimSpace(text) == "" {
		msg := "couldn't read plan"
		if target.slug != "" {
			msg += " " + target.slug
		}
		if err != nil {
			msg += ": " + err.Error()
		}
		return errResult(msg)
	}
	label := target.slug
	if label == "" {
		label = clip(target.title, 40)
	}
	s.toolLine(true, "RecallPlan", label, target.ts.Format("Jan 2 15:04"), false)
	out := text
	if slug == "" && len(refs) > 1 { // defaulted to the latest — surface the others to choose from
		out += "\n\n---\n(That's the most recent plan. Other saved plans, recall by slug: " + recalledList(refs[1:], 8) + ")"
	}
	return textResult(out)
}

// recalledPlan is one plan recall can surface — from the project session log (Text already loaded)
// or the user-level store (loaded lazily by slug).
type recalledPlan struct {
	slug, title string
	ts          time.Time
	text        string // present when from the session log; "" → read from the user store by slug
}

func (p recalledPlan) load() (string, error) {
	if strings.TrimSpace(p.text) != "" {
		return p.text, nil
	}
	return plans.Read(p.slug)
}

// recallablePlans merges the canonical project session-log plans (full text) with the user-level
// store (cross-project), deduped by slug (the session log wins — it carries the text), newest first.
func (s *Session) recallablePlans() []recalledPlan {
	var out []recalledPlan
	seen := map[string]bool{}
	if recs, err := sessionlog.RecentPlans(s.root, 0); err == nil {
		for _, r := range recs {
			out = append(out, recalledPlan{slug: r.Slug, title: r.Title, ts: r.TS, text: r.Text})
			if r.Slug != "" {
				seen[r.Slug] = true
			}
		}
	}
	if refs, err := plans.List(); err == nil {
		for _, r := range refs {
			if r.Slug != "" && seen[r.Slug] {
				continue // already have it (with text) from the session log
			}
			out = append(out, recalledPlan{slug: r.Slug, title: r.Title, ts: r.Saved})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ts.After(out[j].ts) })
	return out
}

// recalledList renders up to max plans as "slug (title)" for a recall listing.
func recalledList(refs []recalledPlan, max int) string {
	parts := make([]string, 0, len(refs))
	for i, r := range refs {
		if i >= max {
			parts = append(parts, fmt.Sprintf("…and %d more", len(refs)-max))
			break
		}
		label := r.slug
		if label == "" {
			label = "(unsaved)"
		}
		if r.title != "" {
			label += " (" + clip(r.title, 50) + ")"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

// savePlan persists the just-presented plan two ways: the user-level store (~/.memcode/plans, reusing
// this plan session's slug so revisions update one file) AND a structured record in the canonical
// per-session log. Two independent recovery paths → recall works even if one is empty. Best-effort:
// a failure never fails the turn (planCtl.LastPlan still drives execution).
func (s *Session) savePlan(plan string) {
	slug, err := plans.Save(s.planCtl.Slug(), plan)
	if err == nil {
		s.planCtl.RecordSaveSlug(slug)
	}
	s.slog.Append(sessionlog.Record{Kind: sessionlog.KindPlanPresented, Text: s.redactor.Redact(plan), Slug: s.planCtl.Slug()})
}

// RunPlan produces a single, detailed plan for task in plan mode — research-only
// tools, the reasoning model, the planning prompt — and returns the plan text.
// It makes NO changes; the interactive TUI is where a plan is iterated on and
// then executed.
func (s *Session) RunPlan(ctx context.Context, task string) (string, error) {
	s.setSessionID(newSessionID())
	s.approvals = s.loadApprovals(ctx)
	s.EnterPlan(ctx, WithTask(task)) // research-only tools + reasoning model; records plan_started
	s.emit(ctx, events.KindAgentSessionStarted, map[string]any{"task": task, "mode": "plan", "model": s.model})

	sys := s.roomSpec(s.planSpec(s.repoOverview(ctx)))
	messages := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: task}}}}
	if _, _, err := s.runLoop(ctx, sys, &messages); err != nil {
		return "", err
	}
	s.notePlanTurn(ctx) // records plan_proposed
	s.emit(ctx, events.KindAgentSessionFinished, map[string]any{"mode": "plan"})
	return s.lastText, nil
}
