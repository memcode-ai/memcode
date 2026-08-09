package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/doctor"
	"github.com/memcode-ai/memcode/internal/wire"
)

var judgeTestTool = wire.ToolDef{Name: "record_test", InputSchema: map[string]any{"type": "object"}}

type judgeOut struct {
	Verdict string `json:"verdict"`
}

func toolResp(name, input string) wire.Response {
	return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
		{Type: "tool_use", Name: name, ID: "t1", Input: json.RawMessage(input)},
	}}
}

// decodeForcedTool: forced tool_use hit; wrong-name blocks skipped; malformed input falls
// through to the prose fallback; prose-only JSON still decodes (doctrine/CLI skew safety).
func TestDecodeForcedTool(t *testing.T) {
	var out judgeOut
	if !decodeForcedTool(toolResp("record_test", `{"verdict":"ok"}`), judgeTestTool, &out) || out.Verdict != "ok" {
		t.Fatalf("tool_use hit failed: %+v", out)
	}
	out = judgeOut{}
	if decodeForcedTool(toolResp("other_tool", `{"verdict":"ok"}`), judgeTestTool, &out) {
		t.Fatal("wrong-name tool_use must not decode")
	}
	out = judgeOut{}
	prose := wire.Response{Blocks: []wire.Block{{Type: "text", Text: "sure: {\"verdict\":\"ok\"} done"}}}
	if !decodeForcedTool(prose, judgeTestTool, &out) || out.Verdict != "ok" {
		t.Fatalf("prose fallback failed: %+v", out)
	}
	out = judgeOut{}
	if decodeForcedTool(wire.Response{Blocks: []wire.Block{{Type: "text", Text: "no json here"}}}, judgeTestTool, &out) {
		t.Fatal("no verdict anywhere must decode to false")
	}
}

// judgeStats.record: timeouts and errors counted separately, ok resets the streak, and
// the notice fires exactly once per failure streak at the threshold.
func TestJudgeStatsCountsAndNotice(t *testing.T) {
	var js judgeStats
	timeoutErr := context.DeadlineExceeded

	if notice, _ := js.record("turn_intent", timeoutErr, true); notice {
		t.Fatal("no notice before the threshold")
	}
	if notice, _ := js.record("followup_intent", errors.New("boom"), false); notice {
		t.Fatal("no notice before the threshold")
	}
	notice, last := js.record("turn_intent", timeoutErr, true)
	if !notice || last == "" {
		t.Fatalf("threshold reached — notice must fire once, got notice=%v last=%q", notice, last)
	}
	if notice, _ := js.record("turn_intent", timeoutErr, true); notice {
		t.Fatal("only ONE notice per streak")
	}

	if notice, _ := js.record("turn_intent", nil, false); notice {
		t.Fatal("success never notices")
	}
	st := js.byMode["turn_intent"]
	if st.OK != 1 || st.Timeout != 3 || st.Err != 0 {
		t.Fatalf("turn_intent counts = %+v", *st)
	}
	if js.consecutiveFails != 0 || js.noticed {
		t.Fatal("ok must reset the streak and re-arm the notice")
	}
	if st := js.byMode["followup_intent"]; st.Err != 1 || st.Timeout != 0 {
		t.Fatalf("followup_intent counts = %+v", *st)
	}
}

// The session-level notice line prints (dim) when the streak threshold is hit.
func TestRecordJudgePrintsNoticeOnStreak(t *testing.T) {
	var out bytes.Buffer
	s := &Session{out: &out}
	for i := 0; i < judgeFailNotice; i++ {
		s.recordJudge("turn_intent", context.DeadlineExceeded, true)
	}
	if !strings.Contains(out.String(), "background classifiers failing") {
		t.Fatalf("expected the streak notice, got %q", out.String())
	}
	if n := strings.Count(out.String(), "background classifiers failing"); n != 1 {
		t.Fatalf("notice must print once per streak, got %d", n)
	}
}

// classifierChecks: no traffic → no rows; clean traffic → OK row; failures → Warn row
// carrying ok/timeout/err and the last failure.
func TestClassifierChecksRows(t *testing.T) {
	s := &Session{}
	if rows := s.classifierChecks(); rows != nil {
		t.Fatalf("no traffic must yield no rows, got %v", rows)
	}
	s.recordJudge("turn_intent", nil, false)
	s.recordJudge("turn_intent", context.DeadlineExceeded, true)
	rows := s.classifierChecks()
	if len(rows) != 1 {
		t.Fatalf("expected one row, got %v", rows)
	}
	r := rows[0]
	if r.Name != "classifier turn_intent" || r.Status != doctor.Warn {
		t.Fatalf("row = %+v", r)
	}
	for _, want := range []string{"1 ok", "1 timeout", "0 err"} {
		if !strings.Contains(r.Detail, want) {
			t.Fatalf("detail %q missing %q", r.Detail, want)
		}
	}

	s2 := &Session{}
	s2.recordJudge("followup_intent", nil, false)
	if rows := s2.classifierChecks(); len(rows) != 1 || rows[0].Status != doctor.OK {
		t.Fatalf("clean traffic must be OK, got %v", rows)
	}
}
