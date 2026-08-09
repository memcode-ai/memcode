package runtime

import (
	"io"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/plan"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
)

// newSess constructs a test Session, wrapping the fake provider in a metered
// llm.Runner the way the real cmd boundary does. Tests call this instead of New so
// they don't each have to thread the gateway.
func newSess(st store.Store, prov provider.ModelProvider, root, model string, mode permissions.Mode, out io.Writer) *Session {
	return New(st, llm.NewRunner(prov), root, model, mode, out)
}

// enterPlanForTest drives the session into plan mode through the REAL machine
// transition — raw phase pokes are impossible now, which is the point: every
// state a test needs is reachable through the same transitions production uses.
func enterPlanForTest(s *Session, task string) {
	var opts []PlanOpt
	if task != "" {
		opts = append(opts, WithTask(task))
	}
	s.planCtl.Enter(s.model, opts...)
}

// armApplyForTest moves the machine plan→approved-apply with the given contract.
func armApplyForTest(s *Session, contract string) {
	enterPlanForTest(s, "")
	s.planCtl.Present(contract)
	s.planCtl.Approve("")
}

// planCtlResearching returns a Controller already in plan mode (via the real transition).
func planCtlResearching() *plan.Controller {
	c := &plan.Controller{}
	c.Enter("")
	return c
}

// planCtlApplying returns a Controller in the apply phase with a contract armed.
func planCtlApplying() *plan.Controller {
	c := planCtlResearching()
	c.Present("1. step one\n2. step two")
	c.Approve("")
	return c
}
