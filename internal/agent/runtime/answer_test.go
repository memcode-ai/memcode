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
	"github.com/memcode-ai/memcode/internal/wire"
)

// captureProvider records the tools advertised on the last request and returns a
// fixed text answer with no tool use.
type captureProvider struct{ lastTools []wire.ToolDef }

func (c *captureProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	c.lastTools = r.Tools
	return wire.Response{
		StopReason: "end_turn",
		Blocks:     []wire.Block{{Type: "text", Text: "the answer lives in foo.go:42"}},
	}, nil
}

func TestAnswerIsReadOnly(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := &captureProvider{}
	sess := newSess(st, cp, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	ans, err := sess.Answer(ctx, "core", "where is X handled?")
	if err != nil {
		t.Fatal(err)
	}
	if ans != "the answer lives in foo.go:42" {
		t.Fatalf("answer = %q", ans)
	}

	// An explorer must NOT be offered edit_file (a true mutator). bash IS offered —
	// it's the read-only inspect shell (mutating commands are rejected per-command).
	for _, d := range cp.lastTools {
		if d.Name == tools.EditFile || d.Name == tools.Todo {
			t.Fatalf("read-only explorer was offered a mutating tool: %s", d.Name)
		}
	}
	// It should have the read tools and the inspect shell.
	if !hasTool(cp.lastTools, tools.ReadFile) || !hasTool(cp.lastTools, tools.Ripgrep) || !hasTool(cp.lastTools, tools.Bash) {
		t.Fatalf("read-only explorer missing read tools / inspect shell: %+v", cp.lastTools)
	}
}

func TestExplorerRejectsMutatingCommand(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.readOnly = true

	// Read-only command runs; mutating command is rejected (explorers can't prompt).
	if r := s.bash(ctx, []byte(`{"command":"git log --oneline -1"}`)); r.isError {
		// git log is Safe; running it may error only if not a repo — tolerate that,
		// the point is it isn't DENIED. (A deny would carry "read-only explorer".)
	}
	r := s.bash(ctx, []byte(`{"command":"rm -rf /tmp/nope"}`))
	out, isErr := r.text(), r.isError
	if !isErr || !strings.Contains(out, "read-only explorer") {
		t.Fatalf("explorer must reject a mutating command, got %q (err=%v)", out, isErr)
	}
}

type captureProviderNil struct{}

func (captureProviderNil) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	return wire.Response{StopReason: "end_turn"}, nil
}

func hasTool(defs []wire.ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

func TestPlanModeBashGate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "allow-all", permissions.ModeAllowAll, io.Discard)
	s.EnterPlan(ctx) // forces ask-semantics for the inspect shell

	asked := 0
	s.approve = func(_ context.Context, req ApprovalRequest) ApprovalDecision {
		asked++
		return ApprovalDecision{Allow: false, Reason: "nope"}
	}

	// A read-only command auto-runs even under allow-all — no prompt.
	_ = s.bash(ctx, []byte(`{"command":"git status"}`))
	if asked != 0 {
		t.Fatalf("a Safe command must not prompt in plan mode, asked=%d", asked)
	}
	// A mutating command is REJECTED outright in plan mode — NOT prompted, NOT run. Plan mode is
	// research-only; edits happen after the user approves and Executes. Regression guard for the
	// "half-execute inside plan mode" bug, where mutations ran through the inspect shell and the
	// plan→approve→apply state machine was bypassed (the agent re-proposed a plan over changes it
	// had already applied to a live remote).
	r := s.bash(ctx, []byte(`{"command":"gcloud run deploy"}`))
	out, isErr := r.text(), r.isError
	if asked != 0 {
		t.Fatalf("a mutating command must NOT prompt in plan mode (it's deferred, not run), asked=%d", asked)
	}
	if !isErr || !strings.Contains(out, "denied") {
		t.Fatalf("a mutating command in plan mode should be denied, got %q (err=%v)", out, isErr)
	}
}

func TestBashPreview(t *testing.T) {
	// stdout preferred; capped at 4 lines, then a "+N lines" note.
	lines := bashPreview("a\nb\nc\nd\ne\nf\ng\nh", "", 0)
	if len(lines) != 5 || lines[4] != "… +4 lines" {
		t.Fatalf("expected 4 lines + a +N note, got %v", lines)
	}
	// A long line is truncated to one row (…), never wrapped.
	long := bashPreview(strings.Repeat("x", 250), "", 0)
	if len(long) != 1 || !strings.HasSuffix(long[0], "…") || len([]rune(long[0])) > 101 {
		t.Fatalf("long line should be clipped to one row with …, got %d runes", len([]rune(long[0])))
	}
	// silent success vs failure.
	if got := bashPreview("", "", 0); len(got) != 1 || got[0] != "(no output)" {
		t.Fatalf("silent success = %v", got)
	}
	if got := bashPreview("", "", 1); len(got) != 1 || got[0] != "exit 1" {
		t.Fatalf("silent failure = %v", got)
	}
	// stderr shown when there's no stdout.
	if got := bashPreview("", "boom", 2); got[0] != "boom" {
		t.Fatalf("stderr preview = %v", got)
	}
}
