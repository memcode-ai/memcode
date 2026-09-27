package anthropic

import (
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// Fable 5.1 and Opus 5.5 answer a forced tool_choice with a 400, which used to
// silently fail every judge, reflection and review verdict pinned to them.
func TestForcedToolChoiceFallsBackToAutoWhereRejected(t *testing.T) {
	verdict := wire.ToolDef{Name: "record_verdict", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}}
	req := func(model string) wire.Request {
		return wire.Request{
			Model:      model,
			Messages:   []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "Is 2+2=4?"}}}},
			Tools:      []wire.ToolDef{verdict},
			ToolChoice: verdict.Name,
		}
	}

	for _, model := range []string{"claude-fable-5-1", "claude-opus-5-5", "claude-opus-5"} {
		p := wireToParams(buildWire(req(model), 4096, false, ""))
		if p.ToolChoice.OfAuto == nil || p.ToolChoice.OfTool != nil {
			t.Errorf("%s: tool_choice = %+v, want auto", model, p.ToolChoice)
		}
		last := p.Messages[len(p.Messages)-1].Content
		if len(last) != 2 || last[1].OfText == nil || !strings.Contains(last[1].OfText.Text, "`record_verdict`") {
			t.Errorf("%s: final user turn = %+v, want an appended instruction naming the tool", model, last)
		}
	}

	p := wireToParams(buildWire(req("claude-sonnet-5"), 4096, false, ""))
	if p.ToolChoice.OfTool == nil || p.ToolChoice.OfTool.Name != "record_verdict" {
		t.Errorf("sonnet: tool_choice = %+v, want forced record_verdict", p.ToolChoice)
	}
	if got := len(p.Messages[0].Content); got != 1 {
		t.Errorf("sonnet: final user turn has %d blocks, want the prompt untouched", got)
	}
}
