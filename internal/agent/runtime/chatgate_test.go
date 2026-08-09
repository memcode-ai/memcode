package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// chatGateProvider mirrors vxui's planGateProvider for the HEADLESS (line-REPL) intake
// gate: a plan_followup_intent call returns the forced verdict; any other request (the
// plan turn itself) returns a fixed ACKTOKEN reply.
type chatGateProvider struct{ verdict string }

func (p chatGateProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	if r.Mode == "plan_followup_intent" {
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
			{Type: "tool_use", Name: "record_plan_relevance", ID: "t1", Input: json.RawMessage(p.verdict)},
		}}, nil
	}
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "ACKTOKEN"}}}, nil
}

// TestChatSubmitDefersSeparateWhilePlanning: the headless Submit path (chat.go's run
// closure) has the same intake gate as the TUI — a SEPARATE verdict parks the message
// instead of running it as a plan-mode turn.
func TestChatSubmitDefersSeparateWhilePlanning(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var out bytes.Buffer
	s := newSess(st, chatGateProvider{`{"related":false,"title":"Fix the flaky CI job"}`}, t.TempDir(), "sonnet", permissions.ModeAsk, &out)
	chat := s.StartChat(ctx)
	s.EnterPlan(ctx, WithTask("migrate the billing service"))

	s.Submit(ctx, chat, "unrelated but the CI job keeps flaking, can you look at it")

	if len(s.planDeferred) != 1 {
		t.Fatalf("expected the message parked, planDeferred=%#v", s.planDeferred)
	}
	if !strings.Contains(out.String(), "separate") {
		t.Fatalf("expected the separate note in output, got %q", out.String())
	}
	if strings.Contains(out.String(), "ACKTOKEN") {
		t.Fatalf("a separate message must not run a turn, output=%q", out.String())
	}
}

// TestChatSubmitRoutesRelatedWhilePlanning: a related verdict flows through to a real
// (plan-mode) turn, exactly like before the gate existed.
func TestChatSubmitRoutesRelatedWhilePlanning(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var out bytes.Buffer
	s := newSess(st, chatGateProvider{`{"related":true,"title":""}`}, t.TempDir(), "sonnet", permissions.ModeAsk, &out)
	chat := s.StartChat(ctx)
	s.EnterPlan(ctx, WithTask("migrate the billing service"))

	s.Submit(ctx, chat, "also make sure the rollback path is covered")

	if len(s.planDeferred) != 0 {
		t.Fatalf("a related message must not be deferred, got %#v", s.planDeferred)
	}
	if !strings.Contains(out.String(), "ACKTOKEN") {
		t.Fatalf("expected the related message to run a turn, output=%q", out.String())
	}
}
