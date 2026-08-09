package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/plan"
	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// enter_plan is the executive's door INTO plan mode (normal chat only); cancel_plan is the
// cancel-only door, offered ONLY while planning. Neither is offered to a read-only explorer.
func TestPlanToolGating(t *testing.T) {
	normal := &Session{planCtl: &plan.Controller{}}
	planning := &Session{planCtl: planCtlResearching()}
	explorer := &Session{planCtl: &plan.Controller{}, readOnly: true}

	if !normal.allowTool(tools.EnterPlan) {
		t.Error("enter_plan must be offered in normal chat")
	}
	if planning.allowTool(tools.EnterPlan) {
		t.Error("enter_plan must NOT be offered while already planning")
	}
	if explorer.allowTool(tools.EnterPlan) {
		t.Error("enter_plan must NOT be offered to a read-only explorer")
	}

	if !planning.allowTool(tools.CancelPlan) {
		t.Error("cancel_plan must be offered while planning")
	}
	if normal.allowTool(tools.CancelPlan) {
		t.Error("cancel_plan must NOT be offered in normal chat")
	}
	if explorer.allowTool(tools.CancelPlan) {
		t.Error("cancel_plan must NOT be offered to a read-only explorer")
	}
}

// The guard branches return BEFORE touching plan state, so a misfired call is a safe no-op:
// exit when not planning, enter when already planning — neither flips the mode.
func TestPlanToolGuardsAreNoOps(t *testing.T) {
	normal := &Session{planCtl: &plan.Controller{}}
	r := normal.cancelPlanTool(context.Background(), nil)
	if r.isError || !strings.Contains(r.text(), "not in plan mode") {
		t.Errorf("cancel_plan when not planning should no-op: %q (isErr=%v)", r.text(), r.isError)
	}
	if normal.planCtl.Planning() {
		t.Error("a no-op exit must not flip plan mode on")
	}

	planning := &Session{planCtl: planCtlResearching()}
	r = planning.enterPlanTool(context.Background(), nil)
	if r.isError || !strings.Contains(r.text(), "already in plan mode") {
		t.Errorf("enter_plan when already planning should no-op: %q (isErr=%v)", r.text(), r.isError)
	}
	if !planning.planCtl.Planning() {
		t.Error("a no-op enter must leave plan mode on")
	}
}
