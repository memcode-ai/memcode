package runtime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// scriptProvider drives one real turn: assistant text + a bash git-commit action,
// then finishes. (The command fails harmlessly in a non-repo temp dir; we only
// care that the ACTION is captured, not its result.)
type scriptProvider struct{ n int }

func (p *scriptProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	if r.Mode == "turn_intent" { // the routing judge is a side call — not part of the scripted turn
		return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: "n/a"}}}, nil
	}
	p.n++
	if p.n == 1 {
		return wire.Response{StopReason: "tool_use", Blocks: []wire.Block{
			{Type: "text", Text: "I'll commit the keyprobe change."},
			{Type: "tool_use", ID: "t1", Name: tools.Bash, Input: json.RawMessage(`{"command":"git commit -m \"keyprobe wip\""}`)},
		}}, nil
	}
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{
		{Type: "text", Text: "Done — committed the keyprobe change."}}}, nil
}

// TestSessionLogDogfood exercises the REAL capture pipeline (StartChat → Submit →
// EndChat) end-to-end, then confirms the on-disk log is faithful and the agent
// can self-recall from it. Run with -v to eyeball the artifacts.
func TestSessionLogDogfood(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := newSess(st, &scriptProvider{}, root, "sonnet", permissions.ModeAsk, io.Discard)
	s.SetApprover(func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true}
	})

	chat := s.StartChat(ctx)
	id := s.SessionID()
	s.Submit(ctx, chat, "commit the keyprobe change and push")
	s.EndChat(ctx)

	recs, err := sessionlog.Recent(root, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("no records written to the episodic log")
	}

	// The canonical log carries the high-signal trail — and NOT raw tool results.
	kinds := map[string]int{}
	for _, r := range recs {
		kinds[r.Kind]++
	}
	for _, want := range []string{
		sessionlog.KindSessionStarted, sessionlog.KindUserMessage, sessionlog.KindAssistantMessage,
		sessionlog.KindToolCall, sessionlog.KindApproval, sessionlog.KindSessionFinished,
	} {
		if kinds[want] == 0 {
			t.Errorf("missing %q in the log; kinds=%v", want, kinds)
		}
	}
	if kinds[sessionlog.KindToolResult] != 0 {
		t.Errorf("tool_result must NOT be in the canonical log, found %d", kinds[sessionlog.KindToolResult])
	}

	// Recall works.
	if c, _ := sessionlog.Commits(root); len(c) == 0 {
		t.Error("Commits() should surface the git commit action")
	}
	sq, _ := sessionlog.Sidequests(root, id)
	if len(sq) != 1 || !strings.Contains(sq[0].Text, "keyprobe") {
		t.Errorf("sidequests wrong: %+v", sq)
	}
	if hits, _ := sessionlog.Search(root, "keyprobe", 0); len(hits) == 0 {
		t.Error("search 'keyprobe' should hit")
	}

	// The agent's self-recall tool returns the L1 checklist recap.
	recap, isErr := s.Intelligence(ctx, "session", "recap")
	if isErr || !strings.Contains(recap, "keyprobe") {
		t.Errorf("recap recall failed: %q", recap)
	}

	// Print the real artifacts (visible under `go test -v`).
	ev, _ := os.ReadFile(filepath.Join(root, ".memcode", "sessions", id, "events.jsonl"))
	tr, _ := os.ReadFile(filepath.Join(root, ".memcode", "sessions", id, "transcript.md"))
	t.Logf("\n=== events.jsonl ===\n%s", ev)
	t.Logf("\n=== transcript.md ===\n%s", tr)
	t.Logf("\n=== session recap (what the agent self-recalls) ===\n%s\n", recap)
}

// TestRecapPrefersPriorSession is the regression for the orientation bug: a fresh
// session is already non-empty (it holds the user's opening message), so a naive
// "current session" recap shadows the prior one and answers "fresh session." A new
// session that has only said "hi" must recap the LAST session that did work.
func TestRecapPrefersPriorSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// One Session object, two chats. scriptProvider is stateful: session A's turns
	// drive the commit; by session B the provider only returns plain end-turns, so B
	// is a realistic "just said hi, did nothing" session.
	s := newSess(st, &scriptProvider{}, root, "sonnet", permissions.ModeAsk, io.Discard)
	s.SetApprover(func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true}
	})

	// Session A — does real work (commits the keyprobe change).
	a := s.StartChat(ctx)
	idA := s.SessionID()
	s.Submit(ctx, a, "commit the keyprobe change and push")
	s.EndChat(ctx)

	// Session B — a fresh session that only greets; no actions recorded.
	b := s.StartChat(ctx)
	idB := s.SessionID()
	s.Submit(ctx, b, "hi")
	if idB == idA {
		t.Fatal("session B should have a distinct id")
	}

	// Default recap from the near-empty session B must surface session A's work,
	// NOT answer "fresh session."
	recap, isErr := s.Intelligence(ctx, "session", "recap")
	if isErr {
		t.Fatalf("recap errored: %q", recap)
	}
	if !strings.Contains(recap, "keyprobe") {
		t.Errorf("default recap from a fresh session should fall through to the prior one; got:\n%s", recap)
	}

	// The explicit `previous` target must do the same regardless of current content.
	prev, isErr := s.Intelligence(ctx, "session", "previous")
	if isErr || !strings.Contains(prev, "keyprobe") {
		t.Errorf("`session previous` should recap session A; got isErr=%v:\n%s", isErr, prev)
	}
	s.EndChat(ctx)

	// And the reader honours exclusion directly.
	if recs, _ := sessionlog.LatestRecentExcluding(root, idB, 0); len(recs) == 0 {
		t.Error("LatestRecentExcluding(B) should return session A's records")
	}
}

// TestColdStartOrientationUsesPriorSession is the #3 regression: the auto-injected
// cold-start orientation (and the /predict episodic signal) must read from the last
// DISTINCT session, never the fresh current one — the same shadowing class as the
// recall fix, one layer up in orientation/prediction.
func TestColdStartOrientationUsesPriorSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := newSess(st, &scriptProvider{}, root, "sonnet", permissions.ModeAsk, io.Discard)
	s.SetApprover(func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true}
	})

	// Session A — real work.
	a := s.StartChat(ctx)
	s.Submit(ctx, a, "commit the keyprobe change and push")
	s.EndChat(ctx)

	// Session B — fresh. Its system prompt must be oriented by A's work, even though
	// B's own (just-created) session is the newest on disk.
	b := s.StartChat(ctx)
	idB := s.SessionID()
	orient := s.priorSessionOrientation(ctx)
	if !strings.Contains(orient, "keyprobe") {
		t.Errorf("cold-start orientation should reflect the PRIOR session, got:\n%q", orient)
	}
	if orient == "" {
		t.Error("expected a non-empty focus digest from the prior session")
	}
	// The injected prompt carries the focus digest.
	if !strings.Contains(b.sys.extra, "OPEN THREADS") || !strings.Contains(b.sys.extra, "keyprobe") {
		t.Error("StartChat should inject the prior-session focus orientation into the system prompt")
	}
	s.EndChat(ctx)

	// The /predict episodic signal honours the same exclusion.
	if recs, _ := sessionlog.LatestRecentExcluding(root, idB, 120); len(recs) == 0 {
		t.Error("predict's episodic source should resolve to session A, not the fresh B")
	}
}
