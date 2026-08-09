package runtime

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/plans"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// TestRecallPlanFromSessionLog: a plan tagged in a PRIOR session's log is recoverable even when the
// user-level store (~/.memcode/plans) is empty — the case that flailed before (a plan from before
// persistence / a wiped store). Recall merges the session log with the store, so it finds it.
func TestRecallPlanFromSessionLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // user-level store is EMPTY
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	s := newSess(st, captureProviderNil{}, root, "sonnet", permissions.ModeAsk, io.Discard)

	w, err := sessionlog.Open(root, "sess_prior")
	if err != nil {
		t.Fatal(err)
	}
	w.Append(sessionlog.Record{Kind: sessionlog.KindPlanPresented, Text: "# Rebuild apps/www\nPhase 1 — foundation", Slug: "calm-cooking-otter"})
	w.Close()

	if r := s.recallPlanTool(ctx, []byte(`{}`)); r.isError || !strings.Contains(r.text(), "Rebuild apps/www") {
		t.Fatalf("recall must find a session-log plan when the store is empty, got %q (err=%v)", r.text(), r.isError)
	}
	if r := s.recallPlanTool(ctx, []byte(`{"slug":"calm-cooking-otter"}`)); r.isError || !strings.Contains(r.text(), "Phase 1") {
		t.Fatalf("recall by slug from the session log failed, got %q (err=%v)", r.text(), r.isError)
	}
}

// TestRecallPlanTool: the agent retrieves saved plans by natural-language intent (no slash
// command). Empty input → the most recent plan; a slug → that specific plan; an unknown slug →
// an error listing what's available; an empty store → a graceful no-op (not an error).
func TestRecallPlanTool(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from the package TestMain HOME and other tests' saved plans
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	if r := s.recallPlanTool(ctx, []byte(`{}`)); r.isError || !strings.Contains(r.text(), "No plans found") {
		t.Fatalf("empty store must be a graceful no-op, got %q (err=%v)", r.text(), r.isError)
	}

	slugA, err := plans.Save("", "# Alpha plan\nstep A")
	if err != nil {
		t.Fatal(err)
	}
	if r := s.recallPlanTool(ctx, []byte(`{}`)); r.isError || !strings.Contains(r.text(), "Alpha plan") {
		t.Fatalf("no-slug recall must return the (only) plan, got %q (err=%v)", r.text(), r.isError)
	}

	slugB, err := plans.Save("", "# Beta plan\nstep B")
	if err != nil {
		t.Fatal(err)
	}
	if r := s.recallPlanTool(ctx, []byte(`{"slug":"`+slugA+`"}`)); r.isError || !strings.Contains(r.text(), "Alpha plan") {
		t.Fatalf("slug A must return Alpha, got %q (err=%v)", r.text(), r.isError)
	}
	if r := s.recallPlanTool(ctx, []byte(`{"slug":"`+slugB+`"}`)); r.isError || !strings.Contains(r.text(), "Beta plan") {
		t.Fatalf("slug B must return Beta, got %q (err=%v)", r.text(), r.isError)
	}
	if r := s.recallPlanTool(ctx, []byte(`{"slug":"nope-nope-nope"}`)); !r.isError || !strings.Contains(r.text(), "No saved plan") {
		t.Fatalf("unknown slug must error and list options, got %q (err=%v)", r.text(), r.isError)
	}
}

// TestExecutePlanToolEntersApply: the model's execute_plan tool flips the state machine out of
// plan mode and INTO the apply phase, pinning the plan as the contract. This is the fix for
// "typed execute didn't enter the execution phase" — the transition is now an explicit, tracked
// tool call rather than the gate silently treating the message as a revision.
func TestExecutePlanToolEntersApply(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "allow-all", permissions.ModeAllowAll, io.Discard)

	s.EnterPlan(ctx)
	s.planCtl.Present("1. edit the migration\n2. push it") // a proposed plan to approve

	r := s.executePlanTool(ctx, nil)
	out, isErr := r.text(), r.isError
	if isErr {
		t.Fatalf("execute_plan should succeed with a pinned plan, got error: %s", out)
	}
	if s.planCtl.Planning() {
		t.Fatal("execute_plan must leave plan mode")
	}
	if !s.planCtl.IsApplying() {
		t.Fatal("execute_plan must enter the apply phase (Applying)")
	}
	if !strings.Contains(s.planCtl.ApplyContract(), "push it") {
		t.Fatalf("the pinned plan must become the apply contract, got %q", s.planCtl.ApplyContract())
	}
}

// TestExecutePlanToolNeedsPlanMode: execute_plan is a no-op error outside plan mode (nothing to
// approve), so a stray call can't kick off an apply.
func TestExecutePlanToolNeedsPlanMode(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "allow-all", permissions.ModeAllowAll, io.Discard)

	r := s.executePlanTool(ctx, nil)
	out := r.text()
	if !strings.Contains(out, "not in plan mode") {
		t.Fatalf("execute_plan outside plan mode should report no plan, got %q", out)
	}
	if s.planCtl.IsApplying() {
		t.Fatal("execute_plan outside plan mode must not arm an apply")
	}
}

// TestExecutePlanBreaksPlanTurnLoop is the regression proof for the double-execution bug:
// once the model calls execute_plan, the plan-turn loop must END immediately and hand off to the
// single chained apply turn — NOT keep looping. Without the break, execute_plan flips Active→false
// so the loop falls through to the normal branch with mutating tools unlocked, the model obeys
// execute_plan's "implement the steps now" result, and the work runs HERE and AGAIN in the apply
// turn (the "still running the verify / doing the work twice after execute" report). We script
// execute_plan as the first model call; if the loop kept going it would make a SECOND call. The
// guard means exactly one call, with the state machine left armed for the apply turn.
func TestExecutePlanBreaksPlanTurnLoop(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, ".state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	calls := 0
	prov := provFunc(func(r wire.Request) wire.Response {
		calls++
		if calls == 1 { // approve + execute the pinned plan
			return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
				{Type: "tool_use", ID: "x1", Name: tools.ExecutePlan, Input: json.RawMessage(`{}`)},
			}}
		}
		// The loop must NEVER reach a second call: that's the plan turn continuing to "execute"
		// after the handoff. End cleanly (so a regression doesn't spin) — the calls==1 assertion
		// below is what fails if the break is gone.
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "(should not happen)"}}}
	})
	s := newSess(st, prov, dir, "opus", permissions.ModeAllowAll, io.Discard)
	s.EnterPlan(ctx)
	s.planCtl.Present("1. do the thing\n2. verify") // a proposed plan to approve

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "execute the plan"}}}}
	if _, _, err := s.runLoop(ctx, promptSpec{mode: "plan"}, &msgs); err != nil {
		t.Fatalf("runLoop: %v", err)
	}

	if calls != 1 {
		t.Fatalf("plan turn made %d model calls — it must STOP after execute_plan (1 call) and hand off to the apply turn, not keep executing", calls)
	}
	if s.planCtl.Planning() {
		t.Error("execute_plan must leave plan mode")
	}
	if !s.planCtl.IsApplying() {
		t.Error("execute_plan must arm the apply phase — the chained apply turn is the single execution")
	}
}

// TestApplyTurnRunsToCompletion is the regression for the plan-loop guard MISFIRING during the
// apply turn. The chained apply turn runs with planCtl.Applying STILL true (chat.go's apply branch
// clears it only AFTER this runLoop returns), so the plan→apply break must be gated on
// startedInPlan — it fires on the transition, never inside the apply turn. Without the gate the
// apply turn bailed after its first tool batch (a couple of reads, then idle — "the plan isn't
// executing"). Here the apply turn does two tool batches before finishing; all three model calls
// must happen.
func TestApplyTurnRunsToCompletion(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	calls := 0
	prov := provFunc(func(r wire.Request) wire.Response {
		calls++
		if calls <= 2 { // the apply turn does real work across multiple batches
			return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
				{Type: "tool_use", ID: "t", Name: tools.ListDir, Input: json.RawMessage(`{"path":"."}`)},
			}}
		}
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "done implementing"}}}
	})
	s := newSess(st, prov, t.TempDir(), "opus", permissions.ModeAllowAll, io.Discard)
	// Simulate the chained apply turn: NOT in plan mode (Active=false), but Applying is set — the
	// exact state chat.go's apply branch calls runLoop in.
	armApplyForTest(s, "1. do the thing\n2. verify")

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: applyExecuteInstruction}}}}
	if _, ok, err := s.runLoop(ctx, promptSpec{mode: "exec"}, &msgs); err != nil || !ok {
		t.Fatalf("apply runLoop: ok=%v err=%v", ok, err)
	}
	if calls != 3 {
		t.Fatalf("the apply turn must run to completion (2 tool batches + finish = 3 calls); got %d — the plan-loop break is misfiring during the apply turn", calls)
	}
}

// TestExecutePlanOfferedOnlyInPlanMode: the tool is advertised to the executive only while
// planning — never in normal chat (where there's no plan) or to read-only explorers.
func TestExecutePlanOfferedOnlyInPlanMode(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "allow-all", permissions.ModeAllowAll, io.Discard)

	if s.allowTool(tools.ExecutePlan) {
		t.Fatal("execute_plan must NOT be offered in normal chat")
	}
	s.EnterPlan(ctx)
	if !s.allowTool(tools.ExecutePlan) {
		t.Fatal("execute_plan must be offered in plan mode")
	}
	s.readOnly = true
	if s.allowTool(tools.ExecutePlan) {
		t.Fatal("execute_plan must NOT be offered to read-only explorers")
	}
}
