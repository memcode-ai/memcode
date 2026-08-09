package vxui

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/theme"
	"github.com/memcode-ai/memcode/internal/wire"
)

// planGateProvider fakes the gateway for the plan-intake-gate tests: a plan_followup_intent
// call returns the forced record_plan_relevance verdict (verdict is the tool's JSON input);
// anything else (an ordinary chat/plan/apply turn) returns a fixed ACKTOKEN reply, mirroring
// fakeProvider elsewhere in this package.
type planGateProvider struct{ verdict string }

func (p planGateProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	if r.Mode == "plan_followup_intent" {
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
			{Type: "tool_use", Name: "record_plan_relevance", ID: "t1", Input: json.RawMessage(p.verdict)},
		}}, nil
	}
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "ACKTOKEN"}}}, nil
}

// blockingGateProvider is planGateProvider with a classify call that BLOCKS until release
// is closed — for the stranded-defer regression: the plan can exit while the relevance
// verdict is still in flight, and the late verdict must route, not park.
type blockingGateProvider struct {
	release chan struct{}
	verdict string
}

func (p *blockingGateProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	if r.Mode == "plan_followup_intent" {
		<-p.release
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
			{Type: "tool_use", Name: "record_plan_relevance", ID: "t1", Input: json.RawMessage(p.verdict)},
		}}, nil
	}
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "ACKTOKEN"}}}, nil
}

// newPlanGateRunner mirrors newRecRunnerCapture (app_test.go) but wires a session backed by
// planGateProvider instead of the package's standard fakeProvider — needed here to
// discriminate the plan_followup_intent classify call from an ordinary turn.
func newPlanGateRunner(t *testing.T, verdict string) (*appState, *runtime.Session, *recBackend, *ui.Runner) {
	return newPlanGateRunnerWith(t, planGateProvider{verdict})
}

func newPlanGateRunnerWith(t *testing.T, prov provider.ModelProvider) (*appState, *runtime.Session, *recBackend, *ui.Runner) {
	t.Helper()
	theme.Set("aurora")
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess := runtime.New(st, llm.NewRunner(prov), t.TempDir(), "fake-model", permissions.ModeAuto, io.Discard)

	var as *appState
	root := &stateCapture{appWidget: appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}, state: &as}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now) // mount + paint → InitState ran → as is live
	return as, sess, be, runner
}

// waitFor polls (draining dispatched UI-thread closures each tick) until cond is true or a
// bounded deadline passes — the same pattern TestSubmitEchoesAndResponds/TestSlashAdvisorDispatches
// use to settle an async goroutine's result onto the UI thread.
func waitFor(t *testing.T, be *recBackend, runner *ui.Runner, now time.Time, cond func() bool) {
	t.Helper()
	for i := 0; i < 200 && !cond(); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition never became true; scrollback=%q", be.recorded())
	}
}

// TestSubmitDefersUnrelatedMessageWhilePlanning: a composer submission the classifier judges
// SEPARATE from the plan being drafted must never reach the scheduler — it's parked
// (DeferWhilePlanning) and surfaced with a brief scrollback note, not answered as plan content.
func TestSubmitDefersUnrelatedMessageWhilePlanning(t *testing.T) {
	as, sess, be, runner := newPlanGateRunner(t, `{"related":false,"title":"Fix the flaky CI job"}`)
	now := time.Now()
	sess.EnterPlan(context.Background(), runtime.WithTask("migrate the billing service"))

	as.submit("unrelated but the CI job keeps flaking, can you look at it")

	waitFor(t, be, runner, now, func() bool {
		return strings.Contains(be.recorded(), "separate")
	})
	if strings.Contains(be.recorded(), "ACKTOKEN") {
		t.Fatalf("a separate message must NOT be routed to a turn (no ACKTOKEN expected).\nrecorded=%q", be.recorded())
	}
	deferred := sess.DrainPlanDeferred()
	if len(deferred) != 1 || deferred[0] != "unrelated but the CI job keeps flaking, can you look at it" {
		t.Fatalf("expected the message parked in planDeferred, got %v", deferred)
	}
	if q := as.sched.Snapshot(); len(q) != 0 {
		t.Fatalf("a deferred message must never reach the scheduler, got queue %v", q)
	}
}

// TestSubmitStillRoutesRelatedMessageWhilePlanning: a message the classifier judges related
// (continues/steers the plan) must still flow through to a real turn, exactly like today.
func TestSubmitStillRoutesRelatedMessageWhilePlanning(t *testing.T) {
	as, sess, be, runner := newPlanGateRunner(t, `{"related":true,"title":""}`)
	now := time.Now()
	sess.EnterPlan(context.Background(), runtime.WithTask("migrate the billing service"))

	as.submit("also make sure the rollback path is covered")

	waitFor(t, be, runner, now, func() bool {
		return strings.Contains(be.recorded(), "ACKTOKEN")
	})
	if deferred := sess.DrainPlanDeferred(); len(deferred) != 0 {
		t.Fatalf("a related message must not be deferred, got %v", deferred)
	}
}

// TestPlanExecuteDrainsDeferredAfterApply: /execute (planExecute) must replay whatever was
// parked while planning — after the just-accepted apply instruction, so it runs once the
// plan's execution is actually done, not lost.
func TestPlanExecuteDrainsDeferredAfterApply(t *testing.T) {
	as, sess, be, runner := newPlanGateRunner(t, `{"related":false,"title":"Check on the CI job"}`)
	now := time.Now()
	sess.EnterPlan(context.Background(), runtime.WithTask("migrate the billing service"))
	sess.DeferWhilePlanning("check on the CI job", "Check on the CI job")

	as.planExecute()

	// DrainPlanDeferred is synchronous — the buffer must be empty immediately.
	if got := sess.DrainPlanDeferred(); len(got) != 0 {
		t.Fatalf("planExecute should have already drained the buffer, got %v", got)
	}
	// Both the apply instruction's turn and the deferred item's turn must eventually run to
	// completion (scheduler back to fully idle) — the deferred item isn't left stranded.
	waitFor(t, be, runner, now, func() bool {
		return !as.busy() && len(as.sched.Snapshot()) == 0
	})
}

// TestPlanCancelDrainsDeferred: /cancel (planCancel) must also replay parked messages —
// nothing is queued ahead of them on cancel, so they start right away.
func TestPlanCancelDrainsDeferred(t *testing.T) {
	as, sess, be, runner := newPlanGateRunner(t, `{"related":false,"title":"Check on the CI job"}`)
	now := time.Now()
	sess.EnterPlan(context.Background(), runtime.WithTask("migrate the billing service"))
	sess.DeferWhilePlanning("check on the CI job", "Check on the CI job")

	as.planCancel()

	if got := sess.DrainPlanDeferred(); len(got) != 0 {
		t.Fatalf("planCancel should have already drained the buffer, got %v", got)
	}
	waitFor(t, be, runner, now, func() bool {
		return strings.Contains(be.recorded(), "ACKTOKEN")
	})
}

// TestPlanCancelWhileClassifyingRoutesInsteadOfStranding: the stranded-defer regression.
// If the plan exits (cancel/execute) while a relevance classify is still in flight, the
// late verdict must ROUTE the message as a normal turn — deferring it then would park it
// after the drain already passed, and the next EnterPlan would silently destroy it.
func TestPlanCancelWhileClassifyingRoutesInsteadOfStranding(t *testing.T) {
	prov := &blockingGateProvider{release: make(chan struct{}), verdict: `{"related":false,"title":"Fix the flaky CI job"}`}
	as, sess, be, runner := newPlanGateRunnerWith(t, prov)
	now := time.Now()
	sess.EnterPlan(context.Background(), runtime.WithTask("migrate the billing service"))

	as.submit("unrelated but the CI job keeps flaking, can you look at it")
	as.planCancel()     // plan exits + drains while the classify call is still blocked
	close(prov.release) // the "separate" verdict lands AFTER the drain

	waitFor(t, be, runner, now, func() bool {
		return strings.Contains(be.recorded(), "ACKTOKEN")
	})
	if strings.Contains(be.recorded(), "separate — queued") {
		t.Fatalf("late verdict must not defer after the plan exited.\nrecorded=%q", be.recorded())
	}
	if got := sess.DrainPlanDeferred(); len(got) != 0 {
		t.Fatalf("nothing may stay parked after the plan exited, got %v", got)
	}
}

// TestPlanStartCarriesLeftoverDeferred: a plan abandoned without a clean TUI exit (its
// drain never ran) must not lose parked messages when the NEXT plan starts — planStart
// captures them before EnterPlan wipes the buffer and re-parks them against the new plan,
// so they still drain at its exit.
func TestPlanStartCarriesLeftoverDeferred(t *testing.T) {
	as, sess, be, runner := newPlanGateRunner(t, `{"related":true,"title":""}`)
	now := time.Now()
	// Simulate the unclean exit: park something, then ExitPlan WITHOUT the TUI drain.
	sess.EnterPlan(context.Background(), runtime.WithTask("old task"))
	sess.DeferWhilePlanning("check on the CI job", "Check on the CI job")
	sess.ExitPlan(context.Background(), false)

	as.planStart("new task", false)

	got := sess.DrainPlanDeferred()
	if len(got) != 1 || got[0] != "check on the CI job" {
		t.Fatalf("leftover must be re-parked against the new plan, got %v", got)
	}
	// Let the plan turn planStart kicked off settle before teardown (it writes session files).
	waitFor(t, be, runner, now, func() bool { return !as.busy() })
}
