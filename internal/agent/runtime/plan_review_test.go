package runtime

import (
	"encoding/json"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// The AUTOMATIC plan-review tests are DELETED with the feature:
// TestPlanReviewRevises, TestPlanRevisionInvestigates, and
// TestPlanSynthEscalatesWhenCheapWhiffs all asserted that a cheap-drafted plan
// was critiqued by a second model and revised or escalated. Plans draft on the
// user's pinned model now, and review is an explicit choice on the approval
// card. The verdict PARSER below survives — the explicit review reuses it.

// parsePlanVerdict prefers the forced record_verdict tool_use (structured), and falls back to
// prose-wrapped JSON for any model/path that emitted text instead.
func TestParsePlanVerdict(t *testing.T) {
	tool := wire.Response{Blocks: []wire.Block{
		{Type: "text", Text: "investigated the refs"},
		{Type: "tool_use", Name: "record_verdict", Input: json.RawMessage(`{"verdict":"ok","summary":"all refs verified"}`)},
	}}
	if v, ok := parsePlanVerdict(tool); !ok || v.Verdict != "ok" || v.Summary == "" {
		t.Fatalf("tool_use verdict not parsed: %+v ok=%v", v, ok)
	}
	text := wire.Response{Blocks: []wire.Block{{Type: "text", Text: "verdict:\n{\"verdict\":\"revise\",\"summary\":\"x\"}"}}}
	if v, ok := parsePlanVerdict(text); !ok || v.Verdict != "revise" {
		t.Fatalf("text-fallback verdict not parsed: %+v ok=%v", v, ok)
	}
	if _, ok := parsePlanVerdict(wire.Response{Blocks: []wire.Block{{Type: "text", Text: "no json here"}}}); ok {
		t.Fatal("no verdict anywhere must return ok=false (caller skips)")
	}
}
