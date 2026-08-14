package runtime

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

// TestGateRecoverableInRepoRmTreatedLikeEdit: an in-repo `rm -rf` (classified catastrophic) is
// recoverable via git, so in AUTO mode it runs WITHOUT prompting — like the edit tool deleting a
// file. An out-of-repo rm keeps the catastrophic floor and still prompts.
func TestGateRecoverableInRepoRmTreatedLikeEdit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil { // make root a git repo
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, &captureProvider{}, root, "sonnet", permissions.ModeAuto, io.Discard)

	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		t.Fatal("must NOT prompt for an in-repo rm in auto mode — it's recoverable, gate it like an edit")
		return Denied("")
	}
	if ok, _, _ := s.gateCommand(ctx, permissions.Dangerous, true, "rm -rf apps/www/tmp", ""); !ok {
		t.Fatal("in-repo rm -rf should auto-run in auto mode (recoverable)")
	}

	prompted := false
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision { prompted = true; return Denied("nope") }
	if ok, _, _ := s.gateCommand(ctx, permissions.Dangerous, true, "rm -rf /tmp/outside-the-repo", ""); ok || !prompted {
		t.Fatalf("out-of-repo rm must keep the catastrophic floor (prompt): ok=%v prompted=%v", ok, prompted)
	}
}

// TestGateNoDowngradeWithoutGit: with no git repo there's no restore net, so an in-repo rm is NOT
// downgraded — it keeps prompting.
func TestGateNoDowngradeWithoutGit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir() // no .git
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, &captureProvider{}, root, "sonnet", permissions.ModeAuto, io.Discard)
	prompted := false
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision { prompted = true; return Denied("") }
	if ok, _, _ := s.gateCommand(ctx, permissions.Dangerous, true, "rm -rf apps/x", ""); ok || !prompted {
		t.Fatalf("without a git repo there's no recovery net — must still prompt: ok=%v prompted=%v", ok, prompted)
	}
}

func TestGateCommandStructuredOutcomes(t *testing.T) {
	ctx := context.Background()

	// Deny with a reason → not allowed, reason propagates to the caller (model).
	s := newTodoSession(t)
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision { return Denied("wrong directory") }
	ok, _, reason := s.gateCommand(ctx, permissions.Medium, false, "go build", "")
	if ok || reason != "wrong directory" {
		t.Fatalf("deny-with-reason: ok=%v reason=%q", ok, reason)
	}

	// Allow with an edited command → run the substitute instead of the original.
	s = newTodoSession(t)
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true, Command: "go vet ./..."}
	}
	ok, cmd, _ := s.gateCommand(ctx, permissions.Medium, false, "go build", "")
	if !ok || cmd != "go vet ./..." {
		t.Fatalf("allow-with-edit: ok=%v cmd=%q", ok, cmd)
	}

	// Interrupt → denied and the turn is marked interrupted (STOP).
	s = newTodoSession(t)
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision { return ApprovalDecision{Interrupt: true} }
	ok, _, _ = s.gateCommand(ctx, permissions.Medium, false, "go build", "")
	if ok || !s.turn.interrupted {
		t.Fatalf("interrupt: ok=%v interrupted=%v", ok, s.turn.interrupted)
	}

	// Redirect ("No, and tell me what to do differently" + feedback) → denied,
	// the feedback propagates as the reason, and the turn is marked REDIRECTED
	// (skip siblings) but NOT interrupted — so the loop continues and the model
	// reads the feedback instead of the turn terminating.
	s = newTodoSession(t)
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Redirect: true, Reason: "use the staging config instead"}
	}
	ok, _, reason = s.gateCommand(ctx, permissions.Medium, false, "go build", "")
	if ok || reason != "use the staging config instead" {
		t.Fatalf("redirect: ok=%v reason=%q", ok, reason)
	}
	if s.turn.interrupted {
		t.Fatal("redirect must NOT interrupt the turn")
	}
	if !s.turn.redirected {
		t.Fatal("redirect must mark the turn redirected (skip siblings, continue)")
	}
}

func TestGateEditDenyReason(t *testing.T) {
	s := newTodoSession(t)
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision { return Denied("not that file") }
	ok, reason := s.gate(context.Background(), permissions.Medium, false, ApprovalRequest{Title: "Edit x", Label: "edit"})
	if ok || reason != "not that file" {
		t.Fatalf("edit deny-with-reason: ok=%v reason=%q", ok, reason)
	}
}

func TestRememberApprovalPersistsAndMatches(t *testing.T) {
	ctx := context.Background()
	s := newTodoSession(t)
	calls := 0
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		calls++
		return ApprovalDecision{Allow: true, Remember: true}
	}
	if ok, _, _ := s.gateCommand(ctx, permissions.Medium, false, "ls -la", ""); !ok {
		t.Fatal("first ls should be allowed")
	}
	// The remembered "ls *" rule pre-approves a later ls — approver not consulted again.
	if ok, _, _ := s.gateCommand(ctx, permissions.Medium, false, "ls /tmp", ""); !ok {
		t.Fatal("second ls should be pre-approved by the remembered rule")
	}
	if calls != 1 {
		t.Fatalf("approver consulted %d times, want 1 (rule should auto-approve)", calls)
	}
}

// TestRememberApprovalHonoredForCatastrophic pins the 2026-07-18 fix: "don't ask
// again for rm" on a CATASTROPHIC prompt used to be silently discarded (the card
// offered it, saved nothing, and the very next rm prompted again — "if a user
// allows rm, rm is allowed"). The rule is now saved TRUSTED, which Match honors
// for catastrophic commands, scoped to this repo as before.
func TestRememberApprovalHonoredForCatastrophic(t *testing.T) {
	ctx := context.Background()
	s := newTodoSession(t)
	calls := 0
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		calls++
		return ApprovalDecision{Allow: true, Remember: true}
	}
	// A catastrophic rm (out-of-repo target): prompts once, remember is HONORED.
	if ok, _, _ := s.gateCommand(ctx, permissions.Dangerous, true, "rm -f /Users/x/Desktop/a.pdf", ""); !ok {
		t.Fatal("first rm should be allowed after approval")
	}
	// The next catastrophic rm matches the trusted remembered rule — no re-prompt.
	if ok, _, _ := s.gateCommand(ctx, permissions.Dangerous, true, "rm -f /Users/x/Desktop/b.pdf", ""); !ok {
		t.Fatal("second rm should be pre-approved by the trusted remembered rule")
	}
	if calls != 1 {
		t.Fatalf("approver consulted %d times, want 1 (trusted rule must cover catastrophic rm)", calls)
	}
}

// "Don't ask again" on an EDIT is scoped to edits this session — NOT a global
// allow-all. A later edit auto-allows (approver not consulted), but a command must
// still prompt: the grant must not leak into command authority.
func TestGateEditRememberScopesToEditsThisSession(t *testing.T) {
	ctx := context.Background()
	s := newTodoSession(t)
	editCalls, cmdCalls := 0, 0
	s.approve = func(_ context.Context, req ApprovalRequest) ApprovalDecision {
		if req.Command != "" { // a command card
			cmdCalls++
			return Denied("command still gated")
		}
		editCalls++ // an edit card
		return ApprovalDecision{Allow: true, Remember: true}
	}
	// First edit: approved + remembered for the session.
	if ok, _ := s.gate(ctx, permissions.Medium, false, ApprovalRequest{Title: "a.go", Label: "Edit file"}); !ok {
		t.Fatal("first edit should be allowed")
	}
	// Second edit: auto-allowed by the session grant — approver NOT consulted again.
	if ok, _ := s.gate(ctx, permissions.Medium, false, ApprovalRequest{Title: "b.go", Label: "Edit file"}); !ok {
		t.Fatal("second edit should be auto-allowed by the edits-this-session grant")
	}
	if editCalls != 1 {
		t.Fatalf("edit approver consulted %d times, want 1 (the grant should auto-allow)", editCalls)
	}
	// A command is NOT covered — the grant is edits-only, never a global allow-all.
	if ok, _, _ := s.gateCommand(ctx, permissions.Medium, false, "go build", ""); ok {
		t.Fatal("a command must still be gated — the edit grant is not an allow-all")
	}
	if cmdCalls != 1 {
		t.Fatalf("command approver consulted %d times, want 1 (command still prompts)", cmdCalls)
	}
}

// A catastrophic edit (e.g. self-heal weakening a test) is never covered by the
// edits-this-session grant — the deterministic floor stands.
func TestGateEditRememberNeverCoversCatastrophic(t *testing.T) {
	ctx := context.Background()
	s := newTodoSession(t)
	s.editsAllowed = true // user already granted "edits this session"
	prompted := false
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		prompted = true
		return Denied("nope")
	}
	ok, _ := s.gate(ctx, permissions.Dangerous, true, ApprovalRequest{Title: "x_test.go", Label: "Change what a test verifies"})
	if ok {
		t.Fatal("a catastrophic edit must not be auto-allowed by the edits grant")
	}
	if !prompted {
		t.Fatal("a catastrophic edit must still reach the approver")
	}
}

func TestRememberPattern(t *testing.T) {
	// Always binary-scoped — matches the card's "don't ask again for <binary>",
	// including pipelines that start with that binary.
	for cmd, want := range map[string]string{
		"ls -la":                         "ls *",
		"find apps -name '*.csv' | head": "find *",
		"grep -r foo . 2>/dev/null":      "grep *",
	} {
		if got := rememberPattern(cmd); got != want {
			t.Errorf("rememberPattern(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// The whole point of "don't ask again": the rule must survive into a brand-new
// memcode session (a new process reopening the same .memcode/state.db).
func TestRememberedApprovalSurvivesNewSession(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Session 1: user picks "Yes, and don't ask again" for an ls command.
	st1, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s1 := newSess(st1, &captureProvider{}, dir, "sonnet", permissions.ModeAsk, io.Discard)
	s1.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true, Remember: true}
	}
	if ok, _, _ := s1.gateCommand(ctx, permissions.Medium, false, "ls -la", ""); !ok {
		t.Fatal("session 1: command should be allowed")
	}
	st1.Close()

	// Session 2: a fresh process — reopen the SAME db and load approvals the way
	// StartChat/Run do at session start.
	st2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	s2 := newSess(st2, &captureProvider{}, dir, "sonnet", permissions.ModeAsk, io.Discard)
	s2.approvals = s2.loadApprovals(ctx)

	prompted := false
	s2.approve = func(context.Context, ApprovalRequest) ApprovalDecision { prompted = true; return ApprovalDecision{} }
	ok, _, _ := s2.gateCommand(ctx, permissions.Medium, false, "ls /tmp/elsewhere", "")
	if !ok {
		t.Fatal("session 2: the remembered rule should auto-approve a new ls command")
	}
	if prompted {
		t.Fatal("session 2: must NOT prompt again — the rule should persist across sessions")
	}
}

// Permission PROVENANCE: an auto-run that would have prompted in ask mode names
// the gate that let it through ("auto-allowed"); Safe reads
// stay silent so attribution never becomes spam.
func TestGateAllowNoteProvenance(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var out bytes.Buffer
	s := newSess(st, &captureProvider{}, t.TempDir(), "sonnet", permissions.ModeAuto, &out)

	if ok, _, _ := s.gateCommand(ctx, permissions.Medium, false, "go build ./...", ""); !ok {
		t.Fatal("medium risk must auto-run in auto mode")
	}
	s.flushAllowNote() // the tool surface flushes after printing its marker
	if !strings.Contains(out.String(), "auto-allowed") {
		t.Fatalf("auto-run must carry the terse provenance mark, got: %q", out.String())
	}

	out.Reset()
	if ok, _, _ := s.gateCommand(ctx, permissions.Safe, false, "ls -la", ""); !ok {
		t.Fatal("safe must run")
	}
	s.flushAllowNote()
	if strings.Contains(out.String(), "allowed") {
		t.Fatalf("safe reads must stay silent, got: %q", out.String())
	}

	// A remembered rule names itself too.
	out.Reset()
	s.approvals = []permissions.Approval{{Pattern: "npm test*"}}
	if ok, _, _ := s.gateCommand(ctx, permissions.Medium, false, "npm test -- --watch=false", ""); !ok {
		t.Fatal("remembered approval must allow")
	}
	s.flushAllowNote()
	if !strings.Contains(out.String(), "pre-approved") {
		t.Fatalf("remembered-rule provenance missing, got: %q", out.String())
	}
}
