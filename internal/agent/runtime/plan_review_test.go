package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// stubReviewRevise: research ends fast, the draft synthesis is a CHEAP plan (Backend=vllm →
// eligible for review), the cross-model review returns "revise", and the revision synthesis
// produces the final plan. Synthesis AND the revision both carry the full toolset now (draftPlan),
// so they're discriminated by the `nudge` fact (set on synthesis/revision, empty on research) —
// NOT by the presence of tools, which was a stale proxy that broke once revisions kept their tools.
type stubReviewRevise struct {
	synth, review   int
	reviewMaxTokens int // captured from the review request — must stay 0 (gateway owns output budget)
}

func (p *stubReviewRevise) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	switch {
	case r.Mode == "review": // the critic's VERDICT — now a FORCED record_verdict tool call
		p.review++
		p.reviewMaxTokens = r.MaxTokens
		if r.ToolChoice != "record_verdict" {
			return wire.Response{}, fmt.Errorf("verdict call must force the record_verdict tool, got %q", r.ToolChoice)
		}
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{{
			Type: "tool_use", Name: "record_verdict",
			Input: json.RawMessage(`{"verdict":"revise","severity":"medium","summary":"no verification step","issues":[{"kind":"missing_step","detail":"no verify"}],"feedback":"add a verification step after the edits"}`),
		}}}, nil
	case r.Facts["nudge"] != "": // synthesis OR revision (both tool-enabled) → returns the plan prose
		p.synth++
		if p.synth == 1 {
			return wire.Response{StopReason: "end_turn", Backend: "vllm", // cheap draft → review fires
				Blocks: []wire.Block{{Type: "text", Text: longPlan("DRAFT PLAN")}}}, nil
		}
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: longPlan("REVISED PLAN")}}}, nil
	case len(r.Tools) > 0: // research/audit turn (no nudge) → end immediately, reach synthesis fast
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "done"}}}, nil
	}
	// reflect / clarify (no nudge, no JSON → no action)
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "not json"}}}, nil
}

// stubReviseInvestigates: the draft is reviewed once ("revise"); the revision then INVESTIGATES
// (a list_dir) before writing the revised plan — proving the revision kept its full toolset and
// isn't a blind one-shot regeneration. A tool-less revision would finish at synth==2 and dead-end.
type stubReviseInvestigates struct {
	synth, review int
}

func (p *stubReviseInvestigates) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	switch {
	case r.Mode == "review":
		p.review++
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{{
			Type: "tool_use", Name: "record_verdict",
			Input: json.RawMessage(`{"verdict":"revise","summary":"verify the dir the reviewer flagged","issues":[{"kind":"unverified","detail":"check the layout before revising"}]}`),
		}}}, nil
	case r.Facts["nudge"] != "": // synthesis / revision (tool-enabled)
		p.synth++
		switch p.synth {
		case 1: // initial draft → cheap (Backend vllm → review fires)
			return wire.Response{StopReason: "end_turn", Backend: "vllm",
				Blocks: []wire.Block{{Type: "text", Text: longPlan("DRAFT PLAN")}}}, nil
		case 2: // revision step 1: INVESTIGATE before revising (the capability under test)
			return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{{
				Type: "tool_use", ID: "t1", Name: tools.ListDir, Input: json.RawMessage(`{"path":"."}`),
			}}}, nil
		default: // revision step 2: now that it has investigated, write the revised plan
			return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: longPlan("REVISED PLAN")}}}, nil
		}
	case len(r.Tools) > 0: // research turn (no nudge)
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "done"}}}, nil
	}
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "not json"}}}, nil
}

// TestPlanRevisionInvestigates locks the gap fix: a "revise" verdict does NOT force a blind,
// tool-less regeneration. The revision keeps its full toolset, so when it needs to investigate
// (here a list_dir to check what the reviewer flagged) it can, THEN writes the revised plan. The
// mid-revision tool call is what makes this three synth calls (draft → investigate → revise); a
// neutered tool-less revision would be two and dead-end at "couldn't auto-resolve".
func TestPlanRevisionInvestigates(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	prov := &stubReviseInvestigates{}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.EnterPlan(ctx)

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "plan it"}}}}
	if _, _, err := s.runLoop(ctx, promptSpec{mode: "plan"}, &msgs); err != nil {
		t.Fatal(err)
	}
	if prov.review != 1 {
		t.Fatalf("expected exactly one review, got %d", prov.review)
	}
	if prov.synth != 3 {
		t.Fatalf("the revision must be tool-enabled (draft + investigate + revise = 3 synth calls); got %d — a tool-less revision would be 2 and dead-end", prov.synth)
	}
	if !strings.Contains(s.lastText, "REVISED PLAN") {
		t.Fatalf("the shown plan should be the revised one (written after investigating), got %q", s.lastText)
	}
	if !s.PlanPresentable() {
		t.Fatal("a successfully revised plan must be presentable")
	}
}

// stubSynthEscalate: both cheap synthesis attempts return a too-short stub; only the
// escalated call (RoutingHint reason plan_synth_incomplete) returns a real plan.
type stubSynthEscalate struct {
	synth     int
	escalated bool
}

func (p *stubSynthEscalate) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	switch {
	case len(r.Tools) > 0: // research/audit turn → end fast
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "done"}}}, nil
	case r.Facts["nudge"] == "": // reflect / clarify
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "not json"}}}, nil
	}
	p.synth++
	if r.RoutingHint != nil && r.RoutingHint.Reason == "plan_synth_incomplete" {
		p.escalated = true // the 2nd backstop fired — Opus lands a real plan
		return wire.Response{StopReason: "end_turn", Backend: "anthropic", Blocks: []wire.Block{{Type: "text", Text: longPlan("ESCALATED PLAN")}}}, nil
	}
	return wire.Response{StopReason: "end_turn", Backend: "vllm", Blocks: []wire.Block{{Type: "text", Text: "too short"}}}, nil // both cheap tries whiff
}

// TestPlanSynthEscalatesWhenCheapWhiffs: when the cheap planner can't produce a usable plan
// even after the regen, synthesis escalates to Opus (RoutingHint plan_synth_incomplete) so a
// plan still lands — instead of leaving plan mode idle with nothing.
func TestPlanSynthEscalatesWhenCheapWhiffs(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	prov := &stubSynthEscalate{}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.EnterPlan(ctx)
	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "plan it"}}}}
	if _, _, err := s.runLoop(ctx, promptSpec{mode: "plan"}, &msgs); err != nil {
		t.Fatal(err)
	}
	if !prov.escalated {
		t.Fatal("two short cheap syntheses must escalate to Opus (plan_synth_incomplete)")
	}
	if !strings.Contains(s.lastText, "ESCALATED PLAN") {
		t.Fatalf("the landed plan should be the escalated one, got %q", s.lastText)
	}
	if !s.PlanPresentable() {
		t.Fatal("an escalated plan must be presentable (selector raises)")
	}
}

// parsePlanVerdict prefers the forced record_verdict tool_use (structured), and falls back to
// prose-wrapped JSON for any model/path that emitted text instead.
func TestParsePlanVerdict(t *testing.T) {
	tool := wire.Response{Blocks: []wire.Block{
		{Type: "text", Text: "investigated the refs"},
		{Type: "tool_use", Name: "record_verdict", Input: json.RawMessage(`{"verdict":"ok","summary":"all refs verified"}`)},
	}}
	if v, ok := parsePlanVerdict(tool); !ok || v.Verdict != "ok" || v.Summary == "" {
		t.Fatalf("tool_use verdict not parsed: %+v ok=%v", v, ok)
	}
	text := wire.Response{Blocks: []wire.Block{{Type: "text", Text: "verdict:\n{\"verdict\":\"revise\",\"summary\":\"x\"}"}}}
	if v, ok := parsePlanVerdict(text); !ok || v.Verdict != "revise" {
		t.Fatalf("text-fallback verdict not parsed: %+v ok=%v", v, ok)
	}
	if _, ok := parsePlanVerdict(wire.Response{Blocks: []wire.Block{{Type: "text", Text: "no json here"}}}); ok {
		t.Fatal("no verdict anywhere must return ok=false (caller skips)")
	}
}

// TestPlanReviewRevises: a cheap-drafted plan is reviewed once; a "revise" verdict triggers
// exactly one revision synthesis, and the FINAL (revised) plan is what's stored.
func TestPlanReviewRevises(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	prov := &stubReviewRevise{}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.EnterPlan(ctx)

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "plan it"}}}}
	if _, _, err := s.runLoop(ctx, promptSpec{mode: "plan"}, &msgs); err != nil {
		t.Fatal(err)
	}
	// REGRESSION GUARD: the review call must NOT carry a CLI-set output budget. Output budget
	// for a gateway-resolved JSON call (the gateway picks the reviewer model + forces JSON
	// output) is the GATEWAY's concern, not the CLI's. Do NOT add MaxTokens to the review
	// request — if this fails, someone (likely me) hardcoded a budget there again. Take it out.
	if prov.reviewMaxTokens != 0 {
		t.Fatalf("the review request must not set MaxTokens (got %d) — the gateway owns output budget for JSON calls; remove it", prov.reviewMaxTokens)
	}
	if prov.review != 1 {
		t.Fatalf("a cheap-drafted plan must be reviewed exactly once, got %d", prov.review)
	}
	if prov.synth != 2 {
		t.Fatalf("a 'revise' verdict must trigger exactly one revision synthesis (got %d synth calls)", prov.synth)
	}
	if !strings.Contains(s.lastText, "REVISED PLAN") {
		t.Fatalf("the shown plan should be the revised one, got %q", s.lastText)
	}
}
