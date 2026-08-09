package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

func tu(id, name, input string) wire.Block {
	return wire.Block{Type: "tool_use", ID: id, Name: name, Input: []byte(input)}
}

func TestExecuteBatchParallelReadsOrdered(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for name, body := range map[string]string{"a.txt": "alpha NEEDLE here", "b.txt": "beta", "c.txt": "gamma"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(ctx, filepath.Join(dir, ".state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, &captureProvider{}, dir, "sonnet", permissions.ModeAsk, io.Discard)
	s.sessionID = "sess_batch"

	// All parallel-safe → executed concurrently; results must stay in order.
	uses := []wire.Block{
		tu("0", tools.ListDir, `{"path":"."}`),
		tu("1", tools.ReadFile, `{"path":"a.txt"}`),
		tu("2", tools.ReadFile, `{"path":"b.txt"}`),
		tu("3", tools.ReadFile, `{"path":"c.txt"}`),
		tu("4", tools.Ripgrep, `{"query":"NEEDLE"}`),
	}
	results := s.executeBatch(ctx, uses)

	if len(results) != len(uses) {
		t.Fatalf("got %d results, want %d", len(results), len(uses))
	}
	for i, r := range results {
		if r.ToolUseID != uses[i].ID {
			t.Fatalf("result %d out of order: id=%q want %q", i, r.ToolUseID, uses[i].ID)
		}
	}
	if !strings.Contains(results[1].Content, "alpha") {
		t.Errorf("read a.txt missing content: %q", results[1].Content)
	}
	if !strings.Contains(results[4].Content, "a.txt") {
		t.Errorf("ripgrep should have found NEEDLE in a.txt: %q", results[4].Content)
	}
	// Guarded counters are correct despite concurrency.
	if s.metrics.filesRead != 3 {
		t.Errorf("filesRead = %d, want 3", s.metrics.filesRead)
	}
	if s.metrics.toolCalls != 5 {
		t.Errorf("toolCalls = %d, want 5", s.metrics.toolCalls)
	}
}

// TestInterruptHaltsRemainingEdits is the regression for "I denied an edit but it
// kept editing": when the user denies one action with Interrupt (stop/redirect),
// the remaining mutating tool calls in the SAME assistant turn must be skipped,
// not applied — every tool_use still gets a paired tool_result.
func TestInterruptHaltsRemainingEdits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, name := range []string{"one.txt", "two.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(ctx, filepath.Join(dir, ".state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, &captureProvider{}, dir, "sonnet", permissions.ModeAsk, io.Discard)
	s.sessionID = "sess_interrupt"
	// User denies the FIRST edit and chooses to stop/redirect (Interrupt).
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Interrupt: true, Reason: "do it differently"}
	}

	uses := []wire.Block{
		tu("0", tools.EditFile, `{"path":"one.txt","old_string":"original","new_string":"CHANGED-1"}`),
		tu("1", tools.EditFile, `{"path":"two.txt","old_string":"original","new_string":"CHANGED-2"}`),
	}
	results := s.executeBatch(ctx, uses)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (every tool_use needs a tool_result)", len(results))
	}
	if !results[1].IsError || !strings.Contains(results[1].Content, "skipped") {
		t.Errorf("second edit should be a skipped tool_result after the interrupt, got %+v", results[1])
	}
	// The denied edit must not have been applied, and the skipped one must be untouched.
	if b, _ := os.ReadFile(filepath.Join(dir, "one.txt")); string(b) != "original" {
		t.Errorf("denied edit was applied anyway: one.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "two.txt")); string(b) != "original" {
		t.Errorf("edit after the interrupt leaked through: two.txt = %q", b)
	}
}

func TestParallelSafeClassification(t *testing.T) {
	for _, n := range []string{tools.ReadFile, tools.ListDir, tools.Ripgrep, tools.GitDiff, tools.Memcode} {
		if !isParallelSafe(n) {
			t.Errorf("%s should be parallel-safe", n)
		}
	}
	for _, n := range []string{tools.EditFile, tools.Bash, tools.Todo} {
		if isParallelSafe(n) {
			t.Errorf("%s must NOT be parallel-safe (mutating)", n)
		}
	}
}

func TestExploreToolReturnsFinding(t *testing.T) {
	s := newTodoSession(t)
	r := s.exploreTool(context.Background(), []byte(`{"question":"how does foo work","scope":"internal/foo"}`))
	out, isErr := r.text(), r.isError
	if isErr {
		t.Fatalf("explore errored: %s", out)
	}
	if !strings.Contains(out, "foo.go") { // the sub-agent's finding
		t.Fatalf("expected the sub-agent finding, got %q", out)
	}
}

type failingExploreProvider struct{}

func (failingExploreProvider) Complete(context.Context, wire.Request) (wire.Response, error) {
	return wire.Response{}, errors.New("scout lane unavailable")
}

func TestExploreToolPrintsFailureReason(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, ".state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var out strings.Builder
	s := newSess(st, failingExploreProvider{}, dir, "sonnet", permissions.ModeAsk, &out)

	r := s.exploreTool(ctx, []byte(`{"question":"what is here","scope":"cli"}`))
	res, isErr := r.text(), r.isError
	if !isErr || !strings.Contains(res, "scout lane unavailable") {
		t.Fatalf("expected returned explore error, got %q err=%v", res, isErr)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "Explore(cli)") ||
		!strings.Contains(rendered, "failed") ||
		!strings.Contains(rendered, "failed: scout lane unavailable") {
		t.Fatalf("expected visible failure reason, got %q", rendered)
	}
}

func TestExploreRejectedInReadOnly(t *testing.T) {
	s := newTodoSession(t)
	s.readOnly = true
	r := s.dispatch(context.Background(), tu("x", tools.Explore, `{"question":"x"}`))
	out, isErr := r.text(), r.isError
	if !isErr || !strings.Contains(out, "nesting") {
		t.Fatalf("a read-only explorer must reject explore (no nesting): %q err=%v", out, isErr)
	}
}

func TestExploreAdvertisedExceptInExplorer(t *testing.T) {
	s := newTodoSession(t)
	// Explorers must NOT be offered explore (no nesting).
	s.readOnly = true
	if hasTool(s.toolDefs(), tools.Explore) {
		t.Error("explorers must NOT be offered the explore tool (no nesting)")
	}
	// Plan mode fans out via explore.
	s.readOnly = false
	enterPlanForTest(s, "")
	if !hasTool(s.toolDefs(), tools.Explore) {
		t.Error("plan mode should be offered the explore tool (that's how it fans out)")
	}
	// Normal chat now ALSO offers explore — so a review can fan out parallel verifiers.
	s.planCtl.Cancel()
	if !hasTool(s.toolDefs(), tools.Explore) {
		t.Error("normal chat should offer explore (verification fan-out)")
	}
}

func TestAskUserToolReturnsAnswer(t *testing.T) {
	s := newTodoSession(t)
	s.ask = func(_ context.Context, req AskRequest) AskResponse {
		if len(req.Options) != 2 {
			t.Errorf("options not passed through: %+v", req.Options)
		}
		return AskResponse{Answer: "use Clerk"}
	}
	r := s.askUserTool(context.Background(), []byte(`{"question":"which auth provider?","options":["Clerk","Auth0"]}`))
	out, isErr := r.text(), r.isError
	if isErr {
		t.Fatalf("ask_user errored: %s", out)
	}
	if !strings.Contains(out, "Clerk") {
		t.Fatalf("the answer should reach the model: %q", out)
	}
}

func TestAskUserRejectedInExplorer(t *testing.T) {
	s := newTodoSession(t)
	s.readOnly = true
	r := s.dispatch(context.Background(), tu("x", tools.AskUser, `{"question":"x"}`))
	out, isErr := r.text(), r.isError
	if !isErr {
		t.Fatalf("a read-only explorer must not ask the user, got %q", out)
	}
	// ask_user is serial (pauses for input), never parallel-safe.
	if isParallelSafe(tools.AskUser) {
		t.Error("ask_user must not be parallel-safe")
	}
}

// TestDispatchToolValidation tests the input validation paths of dispatchTool WITHOUT
// actually spawning a process (empty task and bad mode return before jobs.Spawn).
func TestDispatchToolValidation(t *testing.T) {
	s := newTodoSession(t)

	// Empty task → error before Spawn is called.
	r := s.dispatchTool(context.Background(), []byte(`{"task":""}`))
	out, isErr := r.text(), r.isError
	if !isErr || !strings.Contains(out, "task") {
		t.Fatalf("empty task should error: %q err=%v", out, isErr)
	}

	// Bad mode → error before Spawn is called.
	r = s.dispatchTool(context.Background(), []byte(`{"task":"do something","mode":"ask"}`))
	out, isErr = r.text(), r.isError
	if !isErr || !strings.Contains(out, "auto") {
		t.Fatalf("bad mode should error: %q err=%v", out, isErr)
	}

	// Malformed JSON → error.
	r = s.dispatchTool(context.Background(), []byte(`{not json`))
	out, isErr = r.text(), r.isError
	if !isErr {
		t.Fatalf("malformed JSON should error: %q err=%v", out, isErr)
	}
}

// TestDispatchAdvertisedGating locks the allowTool gates: dispatch is offered in normal
// chat, but NEVER from a read-only explorer or plan mode (it dispatches a MUTATING agent,
// which would bypass the plan→approve gate).
func TestDispatchAdvertisedGating(t *testing.T) {
	s := newTodoSession(t)

	// Normal chat → dispatch is offered.
	if !hasTool(s.toolDefs(), tools.Dispatch) {
		t.Error("normal chat should offer the dispatch tool")
	}

	// Read-only explorer → dispatch is NOT offered (no nesting of mutating agents).
	s.readOnly = true
	if hasTool(s.toolDefs(), tools.Dispatch) {
		t.Error("read-only explorers must NOT be offered dispatch (it spawns a mutating agent)")
	}

	// Plan mode → dispatch is NOT offered (plan mode is research-only; dispatching a
	// mutating agent would bypass the plan→approve→apply gate).
	s.readOnly = false
	enterPlanForTest(s, "")
	if hasTool(s.toolDefs(), tools.Dispatch) {
		t.Error("plan mode must NOT be offered dispatch (bypasses the plan→approve gate)")
	}
}

// TestDispatchDeniedInAskMode: in --ask mode, dispatching an autonomous mutating agent
// must prompt for approval. A denying user stops the dispatch BEFORE any process is
// spawned — the regression for "dispatch bypasses --ask."
func TestDispatchDeniedInAskMode(t *testing.T) {
	s := newTodoSession(t)
	// newTodoSession defaults to ModeAsk.
	called := false
	s.approve = func(_ context.Context, req ApprovalRequest) ApprovalDecision {
		called = true
		if req.Label != "Dispatch sub-agent" {
			t.Errorf("approval label = %q, want 'Dispatch sub-agent'", req.Label)
		}
		return ApprovalDecision{Allow: false, Reason: "not now"}
	}

	r := s.dispatchTool(context.Background(), []byte(`{"task":"refactor the loader"}`))
	out, isErr := r.text(), r.isError
	if !isErr {
		t.Fatalf("denied dispatch should return an error to the model, got %q", out)
	}
	if !called {
		t.Fatal("the approval gate was never consulted — dispatch bypassed --ask")
	}
	if !strings.Contains(out, "denied") || !strings.Contains(out, "not now") {
		t.Fatalf("error should mention denial + reason, got %q", out)
	}
	// No job dir should have been created (Spawn never ran).
	matches, _ := filepath.Glob(filepath.Join(s.root, ".memcode", "jobs", "*"))
	if len(matches) > 0 {
		t.Fatalf("denied dispatch should not have spawned a job, found %v", matches)
	}
}

// TestDispatchAllowedInAskMode: an approving user in --ask mode lets the dispatch
// proceed past the gate. The spawned child (test binary) exits immediately, so this
// mainly locks that the gate passes through and the tool result announces the job.
func TestDispatchAllowedInAskMode(t *testing.T) {
	s := newTodoSession(t)
	s.approve = func(_ context.Context, req ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true}
	}

	r := s.dispatchTool(context.Background(), []byte(`{"task":"tiny task"}`))
	out, isErr := r.text(), r.isError
	if isErr {
		t.Fatalf("approved dispatch should proceed, got error: %q", out)
	}
	if !strings.Contains(out, "dispatched sub-agent") || !strings.Contains(out, "FIRE-AND-FORGET") {
		t.Fatalf("tool result should announce the dispatch + contract, got %q", out)
	}
}

// TestDispatchNoApprovalGateInAutoMode: in auto/allow-all mode, dispatch proceeds
// WITHOUT prompting — the approval gate is a --ask-mode guard only.
func TestDispatchNoApprovalGateInAutoMode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, &captureProvider{}, dir, "sonnet", permissions.ModeAuto, io.Discard)
	s.sessionID = "sess_auto"
	prompted := false
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		prompted = true
		return ApprovalDecision{Allow: true}
	}

	r := s.dispatchTool(ctx, []byte(`{"task":"go go go"}`))
	out, isErr := r.text(), r.isError
	if isErr {
		t.Fatalf("auto-mode dispatch should proceed without error, got: %q", out)
	}
	if prompted {
		t.Fatal("auto mode should NOT prompt for dispatch approval")
	}
}
