package runtime

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// planToolProvider returns a single forced tool_use block carrying the given JSON input —
// the structured-output path ClassifyPlanMessage relies on (record_plan_relevance).
type planToolProvider struct{ input string }

func (p planToolProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
		{Type: "tool_use", Name: "record_plan_relevance", ID: "t1", Input: json.RawMessage(p.input)},
	}}, nil
}

// TestClassifyPlanMessageRelated: a "related" verdict (continues the plan) comes back
// true with no title (title is only meaningful for a separate verdict).
func TestClassifyPlanMessageRelated(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	prov := planToolProvider{`{"related":true,"title":""}`}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	enterPlanForTest(s, "migrate the billing service to the new provider")

	task, draft := s.PlanGateSnapshot()
	related, title := s.ClassifyPlanMessage(ctx, "also make sure the rollback path is covered", task, draft)
	if !related {
		t.Fatal("expected related=true")
	}
	if title != "" {
		t.Fatalf("expected no title on a related verdict, got %q", title)
	}
}

// TestClassifyPlanMessageSeparate: a "separate" verdict comes back false with the
// classifier's synthesized title — never the raw message text.
func TestClassifyPlanMessageSeparate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	prov := planToolProvider{`{"related":false,"title":"Fix the flaky CI job"}`}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	enterPlanForTest(s, "migrate the billing service to the new provider")

	task, draft := s.PlanGateSnapshot()
	related, title := s.ClassifyPlanMessage(ctx, "unrelated but the CI job keeps flaking, can you look at it", task, draft)
	if related {
		t.Fatal("expected related=false")
	}
	if title != "Fix the flaky CI job" {
		t.Fatalf("title = %q, want the classifier's synthesized title", title)
	}
}

// TestClassifyPlanMessageNoTaskDefaultsRelated: with no anchor task (e.g. a test that sets
// planCtl.Active directly without EnterPlan/WithTask), there's nothing to judge against —
// preserve today's fold-in behavior rather than deferring on a guess.
func TestClassifyPlanMessageNoTaskDefaultsRelated(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// A provider that would say "separate" if it were ever called — proving it ISN'T called.
	prov := planToolProvider{`{"related":false,"title":"should never be reached"}`}
	s := newSess(st, prov, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	enterPlanForTest(s, "") // no Task set

	task, draft := s.PlanGateSnapshot()
	related, title := s.ClassifyPlanMessage(ctx, "anything at all", task, draft)
	if !related || title != "" {
		t.Fatalf("no task anchor must default to related=true, title=\"\", got related=%v title=%q", related, title)
	}
}

// TestClassifyPlanMessageFailsOpenOnError: an infra hiccup (timeout/provider error) must
// never silently defer a genuine follow-up — fail open to related=true.
func TestClassifyPlanMessageFailsOpenOnError(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := newSess(st, errProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	enterPlanForTest(s, "migrate the billing service")

	task, draft := s.PlanGateSnapshot()
	related, title := s.ClassifyPlanMessage(ctx, "does this also cover the staging env", task, draft)
	if !related || title != "" {
		t.Fatalf("a classify error must fail open (related=true, no title), got related=%v title=%q", related, title)
	}
}

// TestDeferWhilePlanningSynthesizesTitle: DeferWhilePlanning tracks the parked message on
// the todo list using the classifier's synthesized title, never the raw text, and buffers
// it for later replay by DrainPlanDeferred.
func TestDeferWhilePlanningSynthesizesTitle(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, planToolProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	_ = ctx

	s.DeferWhilePlanning("unrelated but the CI job keeps flaking, can you look at it", "Fix the flaky CI job")

	if len(s.todos) != 1 {
		t.Fatalf("expected one todo item, got %d", len(s.todos))
	}
	if s.todos[0].Title != "Fix the flaky CI job" {
		t.Fatalf("todo title = %q, want the synthesized title, never raw text", s.todos[0].Title)
	}
	if len(s.planDeferred) != 1 || s.planDeferred[0].Text != "unrelated but the CI job keeps flaking, can you look at it" {
		t.Fatalf("planDeferred = %#v, want the raw text buffered for replay", s.planDeferred)
	}
}

// TestDeferWhilePlanningFallsBackToClippedText: when the classifier gave no title (error/
// timeout/parse miss) and the raw text is itself title-shaped (short, single line), the
// todo uses the raw text — never left blank.
func TestDeferWhilePlanningFallsBackToClippedText(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, planToolProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	_ = ctx

	s.DeferWhilePlanning("fix the readme typo", "")

	if len(s.todos) != 1 || s.todos[0].Title != "fix the readme typo" {
		t.Fatalf("expected the raw text as a fallback title, got %#v", s.todos)
	}
}

// TestDeferWhilePlanningNeverPastesLongProse: with no title and a long musing raw text,
// the list gets the fixed fallback label — never 200 chars of pasted user prose. The
// verbatim text is still buffered in planDeferred for replay, so nothing is lost.
func TestDeferWhilePlanningNeverPastesLongProse(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, planToolProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	_ = ctx

	raw := "I'm not sure scripts should be a tool vs a prompt with directions since the llm can use the standard tools to just read write, update, etc? it just needs to make the script executable"
	s.DeferWhilePlanning(raw, "")

	if len(s.todos) != 1 || s.todos[0].Title != followupFallbackTitle {
		t.Fatalf("long prose with no title must get the fallback label, got %#v", s.todos)
	}
	if len(s.planDeferred) != 1 || s.planDeferred[0].Text != raw {
		t.Fatalf("planDeferred = %#v, want the raw text buffered for replay", s.planDeferred)
	}
}

// TestDrainPlanDeferredClearsBuffer: DrainPlanDeferred pops everything FIFO and empties
// the buffer, so a second drain returns nothing.
func TestDrainPlanDeferredClearsBuffer(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, planToolProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	_ = ctx

	s.DeferWhilePlanning("first separate ask", "First separate ask")
	s.DeferWhilePlanning("second separate ask", "Second separate ask")

	got := s.DrainPlanDeferred()
	want := []string{"first separate ask", "second separate ask"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("DrainPlanDeferred() = %v, want %v", got, want)
	}
	if again := s.DrainPlanDeferred(); len(again) != 0 {
		t.Fatalf("second drain should be empty, got %v", again)
	}
}

// TestPlanGateSnapshot: the snapshot returns Task/LastPlan, and stays race-clean against a
// concurrent pin (the loop.go write path) — meaningful under -race, which is exactly the
// bug this API closes: the TUI classify goroutine used to read planCtl fields unlocked.
func TestPlanGateSnapshot(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, planToolProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	enterPlanForTest(s, "migrate the billing service")
	s.planCtl.Present("1. do the thing\n2. verify it")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			s.planCtl.Present("1. do the thing\n2. verify it differently")
		}
	}()
	for i := 0; i < 100; i++ {
		task, _ := s.PlanGateSnapshot()
		if task != "migrate the billing service" {
			t.Fatalf("task = %q", task)
		}
	}
	<-done
}
