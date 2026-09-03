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

// TestAutoResolveUnknowns verifies that autoResolveUnknowns folds user-only unknowns
// as assumptions and non-blocking ones as risks, without calling s.ask.
func TestAutoResolveUnknowns(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	s := newSess(st, fakeProv{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	refl := reflection{
		Decision: "ask_user",
		Unknowns: []unknown{
			{Question: "Which auth provider?", Kind: "user_only", Options: []AskOption{{Label: "Clerk"}, {Label: "Auth0"}}},
			{Question: "What cache TTL?", Kind: "user_only", Options: []AskOption{{Label: "5min"}, {Label: "1hr"}}},
			{Question: "Is the API versioned?", Kind: "non_blocking"},
		},
	}
	got := s.autoResolveUnknowns(ctx, refl)

	if !strings.Contains(got, "Which auth provider?") {
		t.Error("auto-resolve must include the user-only question")
	}
	if !strings.Contains(got, "What cache TTL?") {
		t.Error("auto-resolve must include the second user-only question")
	}
	if !strings.Contains(got, "no human available") {
		t.Error("auto-resolve must include the assumption nudge")
	}
	if !strings.Contains(got, "Is the API versioned?") {
		t.Error("auto-resolve must include non-blocking unknowns as risks")
	}
	if !strings.Contains(got, "Risks") {
		t.Error("auto-resolve must label the risks section")
	}
}

// TestEnterPlanResetsYolo verifies that EnterPlan resets yolo to false before
// applying opts, so a stale yolo flag from a prior plan never leaks into the next one.
func TestEnterPlanResetsYolo(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	s := newSess(st, fakeProv{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	// Enter plan with yolo → flag is true.
	s.EnterPlan(ctx, WithYolo())
	if !s.PlanYolo() {
		t.Fatal("WithYolo() must set plan.yolo = true")
	}
	s.ExitPlan(ctx, false)

	// Enter plan WITHOUT yolo → flag is false (the reset works).
	s.EnterPlan(ctx)
	if s.PlanYolo() {
		t.Fatal("EnterPlan without WithYolo must reset plan.yolo to false")
	}
	s.ExitPlan(ctx, false)

	// Enter plan with yolo again → flag is true (opts still apply after reset).
	s.EnterPlan(ctx, WithYolo())
	if !s.PlanYolo() {
		t.Fatal("second WithYolo() must set plan.yolo = true again")
	}
}

// yoloFinishProvider: research ends fast, reflect returns user_only unknowns,
// and synthesis produces a plan. Under yolo, ask should never be called.
type yoloFinishProvider struct {
	asked bool
}

func (p *yoloFinishProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	switch {
	case r.Mode == "reflect":
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text",
			Text: `{"sufficient":true,"unknowns":[{"question":"Which DB?","kind":"user_only","options":[{"label":"Postgres"},{"label":"SQLite"}]}],"decision":"ask_user"}`}}}, nil
	case r.Facts["nudge"] != "":
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: longPlan("YOLO PLAN")}}}, nil
	default:
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "research done"}}}, nil
	}
}

// TestFinishPlanYoloSuppressesAsk verifies that finishPlan under yolo never calls
// s.ask even when the reflection contains user_only unknowns.
func TestFinishPlanYoloSuppressesAsk(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	defer st.Close()
	prov := &yoloFinishProvider{}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	// Wire a sentinel ask callback — if yolo is broken and asks, this fires.
	s.ask = func(ctx context.Context, req AskRequest) AskResponse {
		prov.asked = true
		return AskResponse{Answer: "SHOULD NOT BE CALLED"}
	}

	s.EnterPlan(ctx, WithYolo())

	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "plan it"}}}}
	if _, ok, err := s.runLoop(ctx, promptSpec{mode: "plan"}, &msgs); err != nil || !ok {
		t.Fatalf("runLoop: ok=%v err=%v", ok, err)
	}
	if prov.asked {
		t.Fatal("yolo finishPlan must NOT call s.ask for user_only unknowns")
	}
	if !strings.Contains(s.lastText, "YOLO PLAN") {
		t.Fatalf("yolo plan should synthesize normally, got %q", s.lastText)
	}
}

type fakeProv struct{}

func (fakeProv) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "ack"}}}, nil
}
