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
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// longPlan returns a realistic-length plan (≥ minPlanLen) carrying a marker, so the
// synthesis stub-guard doesn't mistake a test fixture for a stall.
func longPlan(marker string) string {
	return marker + "\n\nGoal: load the datasets.\nFindings: load_rooms.py:1 writes the DB; staging is separate.\n" +
		"Approach: stage then load, smallest first." + strings.Repeat(" More detail to exceed the stub threshold.", 12) +
		"\nSteps:\n1. Stage.\n2. Validate.\n3. Load.\nRisks: --reset is destructive.\nVerification: row counts match."
}

type recordedCall struct {
	model    string
	purpose  string
	hasTools bool
}

// execProvider scripts the EXECUTIVE architecture: the planner loop delegates a scout
// to an explore sub-agent, then synthesizes. Model selection is server-side now, so
// the executive turns carry NO model (the gateway resolves plan-mode → Opus); the
// scout sub-agent still carries its research model. We branch on PURPOSE + the scout
// model so the fake serves both the parent (main_loop / synth) and the spawned scout.
type execProvider struct{ calls []recordedCall }

func (p *execProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	p.calls = append(p.calls, recordedCall{model: r.Model, purpose: r.Purpose, hasTools: len(r.Tools) > 0})
	if r.Purpose == string(llm.Explore) { // the scout sub-agent (server resolves its tier): answer immediately
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "scout findings"}}}, nil
	}
	// The Opus executive:
	switch {
	case r.Mode == "reflect": // clarify extraction
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "no questions"}}}, nil
	case r.Facts["nudge"] != "": // synthesis
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: longPlan("FINAL PLAN")}}}, nil
	case !p.delegated(): // first agenda turn → delegate research to a scout
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
			{Type: "tool_use", ID: "e1", Name: tools.Explore, Input: json.RawMessage(`{"question":"how does loading work","scope":"apps"}`)},
		}}, nil
	default: // reviewed the scout's findings → ready to plan
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "have enough"}}}, nil
	}
}
func (p *execProvider) delegated() bool {
	for _, c := range p.calls[:len(p.calls)-1] {
		if c.purpose == string(llm.Explore) {
			return true
		}
	}
	return false
}

func TestPlanExecutiveDelegates(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	prov := &execProvider{}
	s := newSess(st, prov, t.TempDir(), "sonnet-research", permissions.ModeAsk, io.Discard)
	s.EnterPlan(ctx)

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "plan it"}}}}
	if _, ok, err := s.runLoop(ctx, promptSpec{mode: "plan"}, &msgs); err != nil || !ok {
		t.Fatalf("runLoop: ok=%v err=%v", ok, err)
	}

	// The executive agenda turn (first call) is a main-loop turn WITH tools — the
	// gateway resolves it to Opus from the plan-mode intent (verified server-side in
	// resolve_test); here we assert the CLI sent the executive turn, not the scout.
	if prov.calls[0].purpose != string(llm.MainLoop) || !prov.calls[0].hasTools {
		t.Fatalf("executive agenda turn should be a main_loop turn WITH tools: %+v", prov.calls[0])
	}
	// Heavy research was DELEGATED to an explore scout sub-agent (the gateway resolves
	// the scout's tier from purpose=explore + plan mode).
	sawScout := false
	for _, c := range prov.calls {
		if c.purpose == string(llm.Explore) {
			sawScout = true
		}
	}
	if !sawScout {
		t.Fatalf("research must be delegated to an explore scout: %+v", prov.calls)
	}
	// The final synthesis is a synth-purpose call that KEEPS its tools (so the planner can do
	// a final read instead of stalling), and it's the last call.
	syn := prov.calls[len(prov.calls)-1]
	if syn.purpose != string(llm.Synth) || !syn.hasTools {
		t.Fatalf("synthesis must be the final synth call WITH tools: %+v", syn)
	}
	if !strings.Contains(s.lastText, "FINAL PLAN") {
		t.Fatalf("plan text = %q", s.lastText)
	}
}

// hitlProvider scripts a plan loop with a load-bearing fork: research, then the
// executive REFLECTION returns a user_only unknown, then the runtime drives the ask
// card, then synthesis. Branches on system text so it serves every phase.
type hitlProvider struct {
	calls    []recordedCall
	toolUsed bool
}

func (p *hitlProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	p.calls = append(p.calls, recordedCall{model: r.Model, purpose: r.Purpose, hasTools: len(r.Tools) > 0})
	switch {
	case r.Mode == "reflect": // executive reflection → a user-only fork
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text",
			Text: `{"sufficient":false,"unknowns":[{"question":"Which slug for govcon_primes?","kind":"user_only","options":["big-primes","federal-integrators"]}],"decision":"ask_user"}`}}}, nil
	case r.Facts["nudge"] != "": // synthesis
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: longPlan("PLAN using big-primes")}}}, nil
	case !p.toolUsed: // research → a tool call
		p.toolUsed = true
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
			{Type: "tool_use", ID: "t1", Name: tools.ListDir, Input: json.RawMessage(`{"path":"."}`)},
		}}, nil
	default: // research → done (no tools) → triggers reflection
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "research done"}}}, nil
	}
}

func TestClarifyGateAsksBeforeSynthesis(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	prov := &hitlProvider{}
	s := newSess(st, prov, t.TempDir(), "sonnet-research", permissions.ModeAsk, io.Discard)
	askedAtCall := -1
	var askedQ string
	s.ask = func(_ context.Context, req AskRequest) AskResponse {
		askedAtCall = len(prov.calls)
		askedQ = req.Question
		return AskResponse{Answer: "big-primes"}
	}
	s.EnterPlan(ctx)

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "plan it"}}}}
	if _, ok, err := s.runLoop(ctx, promptSpec{mode: "plan"}, &msgs); err != nil || !ok {
		t.Fatalf("runLoop: ok=%v err=%v", ok, err)
	}

	// The extracted fork was asked of the user, on the research model (call 3)...
	if askedAtCall != 3 || askedQ != "Which slug for govcon_primes?" {
		t.Fatalf("clarify gate did not ask the extracted fork (askedAtCall=%d q=%q)", askedAtCall, askedQ)
	}
	if prov.calls[askedAtCall-1].purpose == string(llm.Explore) {
		t.Fatalf("extraction must run on the executive, not the delegated scout: %+v", prov.calls[askedAtCall-1])
	}
	// ...strictly BEFORE the synthesis, which is the LAST call (purpose synth), tools ON.
	last := prov.calls[len(prov.calls)-1]
	if last.purpose != string(llm.Synth) || !last.hasTools {
		t.Fatalf("synthesis must be the final synth call WITH tools: %+v", last)
	}
	if askedAtCall >= len(prov.calls) {
		t.Fatal("the ask must occur before the final synthesis call")
	}
}
