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
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// synthReadsThenPlans: research ends, clarify finds nothing, then SYNTHESIS does a final
// targeted read (it now KEEPS its tools) and only then writes the plan. This is the fix for
// the recurring escalation: a tool-less synthesis forced the model to "plan now" while it
// still wanted a read, so it stalled and we escalated to Opus every time. With tools, it
// reads what it needs and plans — no escalation.
type synthReadsThenPlans struct{ synth int }

func (p *synthReadsThenPlans) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	// Synthesis turns carry a nudge — check FIRST, since synthesis now has tools too.
	if r.Facts["nudge"] != "" {
		p.synth++
		if p.synth == 1 { // a final read before planning — tools are available now, so USE one
			return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
				{Type: "text", Text: "one final read before I write the plan"},
				{Type: "tool_use", ID: "s1", Name: tools.ListDir, Input: json.RawMessage(`{"path":"."}`)},
			}}, nil
		}
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: longPlan("SYNTH PLAN")}}}, nil
	}
	if len(r.Tools) > 0 { // research turn → end fast so we reach synthesis
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "done"}}}, nil
	}
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "not json"}}}, nil // clarify: no questions
}

func TestSynthesisKeepsToolsForFinalRead(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	prov := &synthReadsThenPlans{}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.SetPlannerModel("opus")
	s.SetPlanResearchModel("sonnet")
	s.EnterPlan(ctx)

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "plan it"}}}}
	if _, _, err := s.runLoop(ctx, promptSpec{mode: "plan"}, &msgs); err != nil {
		t.Fatal(err)
	}
	// Two synthesis calls: the first DID a final read (tool_use), the second wrote the plan —
	// no escalation, because the model got the tool it needed instead of being forced to stall.
	if prov.synth != 2 {
		t.Fatalf("synthesis should do a final read then plan (2 synth calls), got %d", prov.synth)
	}
	if !strings.Contains(s.lastText, "SYNTH PLAN") {
		t.Fatalf("plan should land after the final read, got %q", s.lastText)
	}
}
