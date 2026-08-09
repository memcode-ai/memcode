package runtime

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

type classifyProvider struct{ json string }

func (p classifyProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: p.json}}}, nil
}

func TestRefineRisk(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mk := func(j string) *Session {
		s := newSess(st, classifyProvider{j}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
		enterPlanForTest(s, "")
		return s
	}

	cases := []struct {
		name string
		json string
		det  permissions.Risk
		want permissions.Risk
	}{
		{"safe_read high conf downgrades", `{"risk":"safe_read","confidence":0.95,"reason":"select"}`, permissions.Medium, permissions.Safe},
		{"probably_read high conf downgrades", `{"risk":"probably_read","confidence":0.9}`, permissions.Medium, permissions.Safe},
		{"safe_read LOW conf stays prompt", `{"risk":"safe_read","confidence":0.5}`, permissions.Medium, permissions.Medium},
		{"probably_write raises", `{"risk":"probably_write","confidence":0.6,"reason":"insert"}`, permissions.Medium, permissions.Dangerous},
		{"dangerous raises", `{"risk":"dangerous","confidence":0.9}`, permissions.Medium, permissions.Dangerous},
		{"unknown stays prompt", `{"risk":"unknown","confidence":0.2}`, permissions.Medium, permissions.Medium},
		// Deterministic Safe/Dangerous are trusted — never sent to the LLM.
		{"det Safe untouched", `{"risk":"dangerous","confidence":1}`, permissions.Safe, permissions.Safe},
		{"det Dangerous untouched", `{"risk":"safe_read","confidence":1}`, permissions.Dangerous, permissions.Dangerous},
	}
	for _, c := range cases {
		if got := mk(c.json).refineRisk(ctx, "some-cmd", c.det); got != c.want {
			t.Errorf("%s: refineRisk(det=%v) = %v, want %v", c.name, c.det, got, c.want)
		}
	}
}

// toolProvider returns a single forced tool_use block carrying the given JSON input —
// the structured-output path ClassifyFollowups relies on.
type toolProvider struct{ input string }

func (p toolProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
		{Type: "tool_use", Name: "record_followups", ID: "t1", Input: json.RawMessage(p.input)},
	}}, nil
}

// ClassifyFollowups is the batched, cheap-model, structured-output classifier the background
// loop runs on the whole queue: related indices are folded, the rest stay queued.
func TestClassifyFollowups(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mk := func(p toolProvider) *Session {
		return newSess(st, p, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	}
	items := []string{"make it 3 implementations", "also fix the readme typo", "use vllm not direct"}

	prov := toolProvider{`{"active_title":"Wrap up the provider abstraction","items":[{"index":0,"related":true},{"index":1,"related":false,"title":"Fix the readme typo"},{"index":2,"related":true}]}`}
	related, activeTitle, ok := mk(prov).ClassifyFollowups(ctx, "wrap up the provider abstraction", items)
	if !ok {
		t.Fatal("a decodable verdict must report ok=true")
	}
	if len(related) != 3 || !related[0].Related || related[1].Related || !related[2].Related {
		t.Fatalf("expected {0,2} related, got %v", related)
	}
	if related[1].Title != "Fix the readme typo" {
		t.Fatalf("expected the synthesized title for the separate item, got %q", related[1].Title)
	}
	if activeTitle != "Wrap up the provider abstraction" {
		t.Fatalf("expected the synthesized title for the active task, got %q", activeTitle)
	}
	// No active task or no items → ok=false (nothing was classified), empty set, no title.
	if got, title, ok := mk(prov).ClassifyFollowups(ctx, "", items); ok || len(got) != 0 || title != "" {
		t.Errorf("no active task must yield ok=false and empty verdicts, got ok=%v %v title=%q", ok, got, title)
	}
	if got, title, ok := mk(prov).ClassifyFollowups(ctx, "task", nil); ok || len(got) != 0 || title != "" {
		t.Errorf("no items must yield ok=false and empty verdicts, got ok=%v %v title=%q", ok, got, title)
	}
	// An out-of-range index from a confused model is ignored, not a panic. The call itself
	// still decoded, so ok=true — the caller leaves the unjudged item queued.
	if got, _, ok := mk(toolProvider{`{"items":[{"index":9,"related":true}]}`}).ClassifyFollowups(ctx, "task", items); !ok || len(got) != 0 {
		t.Errorf("out-of-range index must be ignored (ok=true), got ok=%v %v", ok, got)
	}
}

// capturingToolProvider records the request it served so a test can assert on the
// classify PROMPT, not just the decoded verdict.
type capturingToolProvider struct {
	input string
	last  wire.Request
}

func (p *capturingToolProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	p.last = r
	return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
		{Type: "tool_use", Name: "record_followups", ID: "t1", Input: json.RawMessage(p.input)},
	}}, nil
}

// TestClassifyFollowupsCarriesContext: the judge cannot decide steer-vs-separate (or
// synthesize a sane title) from one anchor sentence — the prompt must carry the recent
// conversation slice from the sessionlog AND the approved plan contract during an apply
// turn. This locks the fix for the incident where "Begin implementing the approved
// plan…" was the ONLY context the classifier ever saw.
func TestClassifyFollowupsCarriesContext(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	w, err := sessionlog.Open(root, "sess_ctx_test")
	if err != nil {
		t.Fatal(err)
	}
	w.Append(sessionlog.Record{Kind: sessionlog.KindUserMessage, Text: "let's add a scripts feature"})
	w.Append(sessionlog.Record{Kind: sessionlog.KindAssistantMessage, Text: "drafting the scripts tool plan now"})
	w.Close()

	prov := &capturingToolProvider{input: `{"items":[{"index":0,"related":true}]}`}
	s := newSess(st, prov, root, "sonnet", permissions.ModeAsk, io.Discard)
	s.setSessionID("sess_ctx_test")
	armApplyForTest(s, "1. build the scripts package\n2. wire the tool registry")

	if _, _, ok := s.ClassifyFollowups(ctx, "Begin implementing the approved plan now.", []string{"hm, tool vs prompt?"}); !ok {
		t.Fatal("classify should succeed")
	}
	prompt := prov.last.Messages[0].Blocks[0].Text
	for _, want := range []string{
		"RECENT CONVERSATION",
		"drafting the scripts tool plan now",
		"APPROVED PLAN BEING EXECUTED",
		"build the scripts package",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("classify prompt missing %q:\n%s", want, prompt)
		}
	}
	if prov.last.MaxTokens != 0 {
		t.Errorf("MaxTokens = %d, want 0 — judges are UNCAPPED (any fixed cap truncates reasoning-lane verdicts; the gateway resolves 0 per backend)", prov.last.MaxTokens)
	}
}

// truncatedProvider simulates the reasoning-lane trap: the model burned the whole output
// budget thinking, so the response stops at max_tokens with no usable tool call.
type truncatedProvider struct{}

func (truncatedProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "max_tokens", Blocks: []wire.Block{{Type: "text", Text: "thinking about"}}}, nil
}

// TestClassifyTruncationIsDistinct: a verdict lost to the max_tokens cap must surface as
// the truncation error (visible in /doctor), not the generic no-verdict — and the caller
// sees ok=false, never fabricated verdicts.
func TestClassifyTruncationIsDistinct(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, truncatedProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	got, _, ok := s.ClassifyFollowups(ctx, "task", []string{"a follow-up"})
	if ok || len(got) != 0 {
		t.Fatalf("truncated verdict must yield ok=false and no verdicts, got ok=%v %v", ok, got)
	}
	stats := s.judges.byMode["followup_intent"]
	if stats == nil || stats.Err != 1 || !strings.Contains(stats.LastErr, "truncated at max_tokens") {
		t.Fatalf("expected one distinct truncation failure recorded, got %+v", stats)
	}
}

func TestAuthorizeCommand(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mk := func(j string) *Session {
		return newSess(st, classifyProvider{j}, t.TempDir(), "sonnet", permissions.ModeAuto, io.Discard)
	}
	cases := []struct {
		name, json, want string
	}{
		{"explicit allow", `{"decision":"allow","reason":"explicit push authorization"}`, "allow"},
		{"unauthorized ask", `{"decision":"ask","reason":"a question, not a directive"}`, "ask"},
		{"hard block", `{"decision":"block","reason":"force push"}`, "block"},
		{"garbage decision → ask", `{"decision":"maybe"}`, "ask"},
		{"no json → no-op", `nope`, ""},
	}
	for _, c := range cases {
		got, _ := mk(c.json).authorizeCommand(ctx, "git push origin main", "commit and push it", "")
		if got != c.want {
			t.Errorf("%s: authorizeCommand = %q, want %q", c.name, got, c.want)
		}
	}
	// no user context → no-op (deterministic gate stands)
	if got, _ := mk(`{"decision":"block"}`).authorizeCommand(ctx, "git push", "", ""); got != "" {
		t.Errorf("empty user request must be a no-op, got %q", got)
	}
}
