package vxui

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	vaxis "github.com/memcode-ai/vaxis"
	"github.com/memcode-ai/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/theme"
	"github.com/memcode-ai/memcode/internal/wire"
)

// advisorProvider answers turns with a plan and serves Advise per mode: "block" waits
// for ctx (a hung gateway), "fail" returns an error.
type advisorProvider struct {
	mode   string
	advise atomic.Int32
}

func (p *advisorProvider) Complete(_ context.Context, _ wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "PLANTEXT step one"}}}, nil
}

func (p *advisorProvider) Advise(ctx context.Context, _, _ string) (string, error) {
	p.advise.Add(1)
	if p.mode == "block" {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "", errors.New("gateway exploded")
}

func newPlanRunner(t *testing.T, prov *advisorProvider) (*appState, *recBackend, *ui.Runner) {
	t.Helper()
	theme.Set("aurora")
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sess := runtime.New(db, llm.NewRunner(prov), t.TempDir(), "fake-model", permissions.ModeAuto, io.Discard)
	t.Cleanup(func() { sess.EndChat(context.Background()) })
	var st *appState
	root := &stateCapture{appWidget: appWidget{ctx: context.Background(), sess: sess, themeName: "aurora"}, state: &st}
	app := ui.NewApp(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
	be := &recBackend{ev: make(chan ui.Event)}
	runner := ui.NewRunner(app, be, nil)
	now := time.Now()
	runner.Start(now)
	_ = runner.HandleFrame(now)
	// One turn so the session has a "plan" (LastText) to send.
	typeSlash(runner, "hi", now)
	pumpUntil(t, be, runner, func() bool { return strings.Contains(be.recorded(), "PLANTEXT") && !st.busy() })
	return st, be, runner
}

func pumpUntil(t *testing.T, be *recBackend, runner *ui.Runner, cond func() bool) {
	t.Helper()
	now := time.Now()
	for i := 0; i < 400 && !cond(); i++ {
		be.drain()
		_ = runner.HandleFrame(now)
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition never held.\nrecorded=%q", be.recorded())
	}
}

// Ctrl+C interrupts a hung plan advisor call (it used to be a dead key: no asyncCancel),
// and the card comes back without the interrupted call's output as advice.
func TestPlanAdvisorCtrlCInterrupts(t *testing.T) {
	prov := &advisorProvider{mode: "block"}
	st, be, runner := newPlanRunner(t, prov)
	st.planAskAdvisor()
	if st.asyncCancel == nil || !st.busy() {
		t.Fatal("advisor call must be a cancellable async op")
	}
	pumpUntil(t, be, runner, func() bool { return prov.advise.Load() == 1 })
	runner.HandleEvent(vaxis.Key{Keycode: 'c', Modifiers: vaxis.ModCtrl}, time.Now())
	pumpUntil(t, be, runner, func() bool { return !st.busy() })
	if !strings.Contains(be.recorded(), "interrupted") {
		t.Fatalf("no interrupt notice.\nrecorded=%q", be.recorded())
	}
	if st.asyncCancel != nil || st.planAdvice != "" || st.planStage != 0 || !st.planChoosing {
		t.Fatalf("after interrupt: cancel=%v advice=%q stage=%d choosing=%v", st.asyncCancel != nil, st.planAdvice, st.planStage, st.planChoosing)
	}
}

// An advisor error is reported as status, never stored as advice the plan would fold in.
func TestPlanAdvisorErrorIsNotAdvice(t *testing.T) {
	prov := &advisorProvider{mode: "fail"}
	st, be, runner := newPlanRunner(t, prov)
	st.planAskAdvisor()
	pumpUntil(t, be, runner, func() bool { return !st.busy() })
	if !strings.Contains(be.recorded(), "gateway exploded") {
		t.Fatalf("advisor error not shown.\nrecorded=%q", be.recorded())
	}
	if st.planAdvice != "" || st.planStage != 0 {
		t.Fatalf("error stored as advice: advice=%q stage=%d", st.planAdvice, st.planStage)
	}
}

// Chained startSpinner calls share one spinner goroutine, which exits (and frees the
// slot) once the app is idle.
func TestStartSpinnerSingleInstance(t *testing.T) {
	prov := &advisorProvider{mode: "block"}
	st, be, runner := newPlanRunner(t, prov)
	st.planAskAdvisor() // busy + startSpinner
	if !spinnerRunning(st) {
		t.Fatal("spinner not running while busy")
	}
	if tok := claimSpinner(st); tok != nil {
		t.Fatal("a second spinner could be claimed while one runs")
	}
	st.startSpinner() // chained turn: no-op
	st.asyncCancel()
	pumpUntil(t, be, runner, func() bool { return !st.busy() })
	pumpUntil(t, be, runner, func() bool { return !spinnerRunning(st) })
}
