package runtime

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// reflectProvider: research ends; the FIRST reflection says research_more (a
// tool-answerable unknown), the SECOND says synthesize.
type reflectProvider struct {
	research int
	reflects int
}

func (p *reflectProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	switch {
	case r.Mode == "reflect":
		p.reflects++
		if p.reflects == 1 {
			return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text",
				Text: `{"sufficient":false,"unknowns":[{"question":"how does the loader write?","kind":"tool_answerable","next_action":"explore loader"}],"decision":"research_more"}`}}}, nil
		}
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text",
			Text: `{"sufficient":true,"unknowns":[],"decision":"synthesize"}`}}}, nil
	case r.Facts["nudge"] != "":
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: longPlan("REFLECTED PLAN")}}}, nil
	default: // research turn → just end (no tools) so we reach reflection fast
		p.research++
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "research turn"}}}, nil
	}
}

func TestReflectGateResearchMore(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	prov := &reflectProvider{}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.SetPlannerModel("opus")
	s.SetPlanResearchModel("sonnet")
	s.EnterPlan(ctx)

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "plan it"}}}}
	if _, ok, err := s.runLoop(ctx, promptSpec{mode: "plan"}, &msgs); err != nil || !ok {
		t.Fatalf("runLoop: ok=%v err=%v", ok, err)
	}
	// The first reflection sent it back to research exactly once.
	if s.planCtl.ReflectRounds() != 1 {
		t.Fatalf("expected 1 reflect-driven research round, got %d", s.planCtl.ReflectRounds())
	}
	if prov.research < 2 {
		t.Fatalf("expected ≥2 research turns (initial + one more after reflection), got %d", prov.research)
	}
	if prov.reflects != 2 {
		t.Fatalf("expected 2 reflections (research_more then synthesize), got %d", prov.reflects)
	}
	if !strings.Contains(s.lastText, "REFLECTED PLAN") {
		t.Fatalf("final plan = %q", s.lastText)
	}
}
