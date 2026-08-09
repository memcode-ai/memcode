package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// planScript: research turn calls a tool, next research turn ends with prose+no-tool,
// clarify gate asks nothing, synthesis writes the plan.
type planScript struct{ calls int }

func (p *planScript) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	p.calls++
	switch p.calls {
	case 1: // research → a read tool
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
			{Type: "tool_use", ID: "t1", Name: tools.ListDir, Input: json.RawMessage(`{"path":"."}`)},
		}}, nil
	case 2: // research → confused prose, no tools (should be SUPPRESSED)
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "RESEARCH_NARRATION the bash output is empty, let me try differently"}}}, nil
	case 3: // clarify gate → nothing to ask
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "no questions"}}}, nil
	default: // synthesis → the PLAN (should be SHOWN)
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: longPlan("## PLAN\n1. Load datasets\n2. Verify")}}}, nil
	}
}

func TestPlanRenders(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	var out bytes.Buffer
	s := newSess(st, &planScript{}, t.TempDir(), "sonnet-research", permissions.ModeAsk, &out)
	s.SetPlannerModel("opus-planner")
	s.SetPlanResearchModel("sonnet-research")
	s.EnterPlan(ctx)

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "plan it"}}}}
	if _, _, err := s.runLoop(ctx, promptSpec{mode: "chat"}, &msgs); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Logf("OUTPUT:\n%s", got)
	if !strings.Contains(got, "## PLAN") || !strings.Contains(got, "Load datasets") {
		t.Errorf("PLAN TEXT NOT RENDERED")
	}
	if strings.Contains(got, "RESEARCH_NARRATION") {
		t.Errorf("research prose leaked (should be suppressed)")
	}
	if !strings.Contains(s.lastText, "## PLAN") || !strings.Contains(s.lastText, "Load datasets") {
		t.Errorf("lastText = %q", s.lastText)
	}
}
