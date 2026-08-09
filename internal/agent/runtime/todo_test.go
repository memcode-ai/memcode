package runtime

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/todos"
	"github.com/memcode-ai/memcode/internal/wire"
)

func newTodoSession(t *testing.T) *Session {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := newSess(st, &captureProvider{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.sessionID = "sess_test"
	return s
}

func (s *Session) todo(t *testing.T, input string) toolResult {
	t.Helper()
	return s.todoTool(context.Background(), []byte(input))
}

func TestTodoCreateAddDoneFlow(t *testing.T) {
	s := newTodoSession(t)

	if r := s.todo(t, `{"action":"create","items":[{"title":"inspect loader"},{"title":"add upsert"}]}`); r.isError {
		t.Fatal("create errored")
	}
	if todos.ActiveIndex(s.todos) != 0 {
		t.Fatalf("first item should be active: %+v", s.todos)
	}
	// add pushes without resending the whole list.
	if r := s.todo(t, `{"action":"add","items":[{"title":"update tests"}]}`); r.isError {
		t.Fatal("add errored")
	}
	if len(s.todos) != 3 {
		t.Fatalf("expected 3 items after add, got %d", len(s.todos))
	}
	// done advances to the next pending item.
	if r := s.todo(t, `{"action":"done"}`); r.isError {
		t.Fatal("done errored")
	}
	if s.todos[0].Status != todos.StatusDone || s.todos[1].Status != todos.StatusActive {
		t.Fatalf("done should complete item 1 and advance: %+v", s.todos)
	}

	// The snapshot is persisted to the event log (provenance), and Current reads it back.
	got, err := todos.Current(context.Background(), s.store)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Status != todos.StatusDone {
		t.Fatalf("event snapshot mismatch: %+v", got)
	}
}

// TestTodoBatchDone: a holistic sweep can mark several items done in ONE call,
// so the tracker stays honest instead of limping behind the work.
func TestTodoBatchDone(t *testing.T) {
	s := newTodoSession(t)
	s.todo(t, `{"action":"create","items":[{"title":"a"},{"title":"b"},{"title":"c"},{"title":"d"}]}`)
	// Reviewed all four in one pass → mark 1,2,4 done in a single call (3 left active).
	if r := s.todo(t, `{"action":"done","indices":[1,2,4]}`); r.isError {
		t.Fatal("batch done errored")
	}
	want := []string{todos.StatusDone, todos.StatusDone, todos.StatusActive, todos.StatusDone}
	for i, w := range want {
		if s.todos[i].Status != w {
			t.Fatalf("item %d status = %v, want %v (full: %+v)", i+1, s.todos[i].Status, w, s.todos)
		}
	}
}

func TestTodoFinalDoneRequiresVerification(t *testing.T) {
	s := newTodoSession(t)
	s.todo(t, `{"action":"create","items":[{"title":"only step"}]}`)

	// Simulate an unverified edit (an edit with no passing verification after it).
	s.metrics.didEdit = true
	s.metrics.lastEditSeq = 5
	s.metrics.lastVerifyOKSeq = 0

	if r := s.todo(t, `{"action":"done"}`); !r.isError {
		t.Fatal("final done with no verification should be rejected")
	}
	if s.todos[0].Status == todos.StatusDone {
		t.Fatal("the item must NOT be marked done when rejected")
	}

	// Now record a passing verification AFTER the edit; the final done should pass.
	s.metrics.lastVerifyOKSeq = 6
	if r := s.todo(t, `{"action":"done"}`); r.isError {
		t.Fatal("final done after verification should be allowed")
	}
	if !s.todos.AllSettled() {
		t.Fatalf("list should be complete: %+v", s.todos)
	}
}

func TestTodoDoneBeforeCreateErrors(t *testing.T) {
	s := newTodoSession(t)
	if r := s.todo(t, `{"action":"done"}`); !r.isError {
		t.Fatal("done with no list should error")
	}
}

func TestPlanModeWithholdsMutatingTools(t *testing.T) {
	s := newTodoSession(t)
	s.EnterPlan(context.Background())
	if !s.Planning() {
		t.Fatal("EnterPlan should set planning")
	}

	advertised := map[string]bool{}
	for _, d := range s.toolDefs() {
		advertised[d.Name] = true
	}
	// edit_file/todo are withheld; bash IS advertised as the gated inspect shell.
	for _, mut := range []string{tools.EditFile, tools.Todo} {
		if advertised[mut] {
			t.Errorf("plan mode must NOT advertise %q", mut)
		}
	}
	for _, ro := range []string{tools.ReadFile, tools.ListDir, tools.Ripgrep, tools.GitDiff, tools.Memcode, tools.Bash} {
		if !advertised[ro] {
			t.Errorf("plan mode should advertise the research tool %q", ro)
		}
	}

	// A mutating tool is rejected even if the model proposes it anyway.
	u := wire.Block{Type: "tool_use", Name: tools.EditFile, Input: []byte(`{"path":"x","new_string":"y"}`)}
	if r := s.dispatch(context.Background(), u); !r.isError || !strings.Contains(r.text(), "plan mode") {
		t.Fatalf("edit in plan mode should be rejected, got %q (isErr=%v)", r.text(), r.isError)
	}

	// Leaving plan mode restores the model and the full tool set.
	s.ExitPlan(context.Background(), true)
	if s.Planning() {
		t.Fatal("ExitPlan should clear planning")
	}
	if !hasTool(s.toolDefs(), tools.EditFile) {
		t.Error("edit_file should be back after leaving plan mode")
	}
}

func TestTodoReadOnlyExplorerRejects(t *testing.T) {
	s := newTodoSession(t)
	s.readOnly = true
	u := wire.Block{Type: "tool_use", Name: tools.Todo, Input: []byte(`{"action":"create","items":[{"title":"x"}]}`)}
	r := s.dispatch(context.Background(), u)
	out, isErr := r.text(), r.isError
	if !isErr || !strings.Contains(out, "read-only") {
		t.Fatalf("read-only explorer should reject todo, got %q (isErr=%v)", out, isErr)
	}
}

// TestNoteSeparateRequestsSeedsPlaceholderWhenEmpty: the FIRST separate-note in a session
// with no todo list yet must seed a placeholder item for the CURRENT task (marked active,
// from activeText) BEFORE the new separate item(s) — otherwise todos.Append's "promote the
// first pending item to active" would wrongly promote the separate ask ahead of the work
// actually in progress.
func TestNoteSeparateRequestsSeedsPlaceholderWhenEmpty(t *testing.T) {
	s := newTodoSession(t)
	var messages []wire.Message
	s.noteSeparateRequests(context.Background(), "fix the auth bug", "", []separateAsk{{Text: "add a dashboard page"}}, &messages)

	if len(s.todos) != 2 {
		t.Fatalf("expected [placeholder, separate item], got %+v", s.todos)
	}
	if s.todos[0].Title != "fix the auth bug" || s.todos[0].Status != todos.StatusActive {
		t.Fatalf("item 1 should be the active placeholder from activeText, got %+v", s.todos[0])
	}
	if s.todos[1].Title != "add a dashboard page" || s.todos[1].Status != todos.StatusPending {
		t.Fatalf("item 2 should be the pending separate item, got %+v", s.todos[1])
	}

	// The FYI note must land in *messages so the model can acknowledge it.
	if len(messages) != 1 || !strings.Contains(messages[0].Blocks[0].Text, "add a dashboard page") {
		t.Fatalf("expected the FYI note appended to messages, got %+v", messages)
	}
}

// TestNoteSeparateRequestsUsesSynthesizedActiveTitle: the concrete bug this locks — the
// placeholder seed for the ACTIVE task must use the classifier's synthesized activeTitle
// (same call as the per-item verdicts) rather than clipping the raw activeText, exactly
// like separate items already get a synthesized title instead of their verbatim text.
func TestNoteSeparateRequestsUsesSynthesizedActiveTitle(t *testing.T) {
	s := newTodoSession(t)
	var messages []wire.Message
	rawActive := "ok so first let's chase down why the auth middleware keeps dropping the session cookie on refresh"
	s.noteSeparateRequests(context.Background(), rawActive, "Fix session cookie dropped on refresh", []separateAsk{{Text: "add a dashboard page"}}, &messages)

	if len(s.todos) != 2 {
		t.Fatalf("expected [placeholder, separate item], got %+v", s.todos)
	}
	if s.todos[0].Title != "Fix session cookie dropped on refresh" || s.todos[0].Status != todos.StatusActive {
		t.Fatalf("placeholder should use the synthesized activeTitle, got %+v", s.todos[0])
	}
	if strings.Contains(s.todos[0].Title, "chase down") {
		t.Fatalf("placeholder must not be the verbatim raw activeText: %q", s.todos[0].Title)
	}
}

// TestNoteSeparateRequestsAppendsWhenListExists: with an existing list (already has an
// active item), no placeholder is seeded — no duplicate, existing active item untouched —
// and the separate item(s) just append as pending.
func TestNoteSeparateRequestsAppendsWhenListExists(t *testing.T) {
	s := newTodoSession(t)
	s.todo(t, `{"action":"create","items":[{"title":"inspect loader"},{"title":"add upsert"}]}`)
	if todos.ActiveIndex(s.todos) != 0 || s.todos[0].Title != "inspect loader" {
		t.Fatalf("precondition: item 1 should be active, got %+v", s.todos)
	}

	var messages []wire.Message
	s.noteSeparateRequests(context.Background(), "inspect loader", "", []separateAsk{{Text: "fix the readme typo"}}, &messages)

	if len(s.todos) != 3 {
		t.Fatalf("expected 3 items (no duplicate placeholder), got %+v", s.todos)
	}
	if s.todos[0].Title != "inspect loader" || s.todos[0].Status != todos.StatusActive {
		t.Fatalf("existing active item must stay untouched, got %+v", s.todos[0])
	}
	if s.todos[2].Title != "fix the readme typo" || s.todos[2].Status != todos.StatusPending {
		t.Fatalf("the separate item should append as pending, got %+v", s.todos[2])
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Blocks[0].Text, "fix the readme typo") {
		t.Fatalf("expected the FYI note appended to messages, got %+v", messages)
	}
}

// TestNoteSeparateRequestsUsesSynthesizedTitle: when the classifier supplied a Title, the
// todo item's title is the SYNTHESIZED one, not the raw verbatim text — the concrete bug
// this locks: a todo list of pasted user sentences is unreadable, so a concise imperative
// title must be used. The raw text still appears verbatim in the FYI note (buildSeparateNote
// quotes the user's actual words back to the model), so both are checked independently.
func TestNoteSeparateRequestsUsesSynthesizedTitle(t *testing.T) {
	s := newTodoSession(t)
	var messages []wire.Message
	raw := "btw which should be allow automatically, along with any other read only or harmless shell commands, it shouldn't prompt for that"
	s.noteSeparateRequests(context.Background(), "do the active work", "", []separateAsk{{Text: raw, Title: "Auto-allow read-only shell commands"}}, &messages)

	if len(s.todos) != 2 {
		t.Fatalf("expected [placeholder, separate item], got %+v", s.todos)
	}
	if s.todos[1].Title != "Auto-allow read-only shell commands" {
		t.Fatalf("todo title should be the synthesized title, got %q", s.todos[1].Title)
	}
	if strings.Contains(s.todos[1].Title, "btw") {
		t.Fatalf("todo title must not be the verbatim raw text: %q", s.todos[1].Title)
	}
	// The FYI note to the model still carries the verbatim text.
	if len(messages) != 1 || !strings.Contains(messages[0].Blocks[0].Text, raw) {
		t.Fatalf("FYI note should still quote the raw text verbatim, got %+v", messages)
	}
}

// TestNoteSeparateRequestsFallsBackToRawTextWhenNoTitle: when the classifier didn't supply
// a title (Title is "") but the raw text is itself title-shaped (short, single line), the
// todo uses the raw text rather than dropping the item or leaving an empty title.
func TestNoteSeparateRequestsFallsBackToRawTextWhenNoTitle(t *testing.T) {
	s := newTodoSession(t)
	var messages []wire.Message
	s.noteSeparateRequests(context.Background(), "do the active work", "", []separateAsk{{Text: "fix the flaky test"}}, &messages)

	if len(s.todos) != 2 || s.todos[1].Title != "fix the flaky test" {
		t.Fatalf("expected the fallback raw-text title, got %+v", s.todos)
	}
}

// TestNoteSeparateRequestsNeverPastesLongProse locks the ENFORCED half of "synthesized,
// not verbatim" — the incident this fixes: no title from the classifier plus a long musing
// raw text used to clip 200 chars of user prose onto the list. Now anything that isn't
// title-shaped gets the fixed fallback label, and the verbatim text appears ONLY in the
// FYI note the model reads.
func TestNoteSeparateRequestsNeverPastesLongProse(t *testing.T) {
	s := newTodoSession(t)
	var messages []wire.Message
	raw := "I'm not sure scripts should be a tool vs a prompt with directions since the llm can use the standard tools to just read write, update, etc? it just needs to make the script executable and there might be more to it"
	s.noteSeparateRequests(context.Background(), "do the active work", "", []separateAsk{{Text: raw}}, &messages)

	if len(s.todos) != 2 {
		t.Fatalf("expected [placeholder, separate item], got %+v", s.todos)
	}
	if s.todos[1].Title != followupFallbackTitle {
		t.Fatalf("long prose with no title must get the fallback label, got %q", s.todos[1].Title)
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Blocks[0].Text, raw) {
		t.Fatalf("FYI note should still quote the raw text verbatim, got %+v", messages)
	}
}

// TestNoteSeparateRequestsRejectsEchoedTitle: a "title" that is the raw message echoed
// back (over-long or multi-line) is not a synthesis — the sink must refuse it, not
// render it. Guards against the cheap model pasting the text into the title field.
func TestNoteSeparateRequestsRejectsEchoedTitle(t *testing.T) {
	s := newTodoSession(t)
	var messages []wire.Message
	raw := "I'm not sure scripts should be a tool vs a prompt with directions since the llm can use the standard tools to just read write, update, etc? it just needs to make the script executable"
	s.noteSeparateRequests(context.Background(), "do the active work", "", []separateAsk{{Text: raw, Title: raw}}, &messages)

	if s.todos[1].Title != followupFallbackTitle {
		t.Fatalf("an echoed over-long title must be rejected for the fallback label, got %q", s.todos[1].Title)
	}
}

// TestSynthTitle pins the guard's exact boundaries.
func TestSynthTitle(t *testing.T) {
	long := strings.Repeat("x", rawTitleMax+1)
	cases := []struct{ title, raw, want string }{
		{"Add a dashboard page", "whatever raw text", "Add a dashboard page"}, // synthesized title wins
		{"", "fix the readme typo", "fix the readme typo"},                    // short raw reads as a title
		{"", long, ""}, // long raw → caller's label
		{strings.Repeat("y", synthTitleMax+1), "fix it", "fix it"}, // echoed title rejected, short raw ok
		{"line one\nline two", long, ""},                           // multi-line title rejected
		{"", "", ""},
	}
	for _, c := range cases {
		if got := synthTitle(c.title, c.raw); got != c.want {
			t.Fatalf("synthTitle(%.20q, %.20q) = %q, want %q", c.title, c.raw, got, c.want)
		}
	}
}
