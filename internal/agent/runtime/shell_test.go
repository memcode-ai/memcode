package runtime

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"
)

// TestShellLane drives the `$` direct-shell lane end-to-end through Submit: the
// command runs verbatim (no model), its output renders, and it's recorded in the
// episodic log so recall/commits see commands run by hand.
func TestShellLane(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var out bytes.Buffer
	s := newSess(st, &scriptProvider{}, root, "sonnet", permissions.ModeAsk, &out)
	s.SetApprover(func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true}
	})

	chat := s.StartChat(ctx)
	id := s.SessionID()
	s.Submit(ctx, chat, "$ echo hello-from-shell")

	if !strings.Contains(out.String(), "hello-from-shell") {
		t.Errorf("shell output should render in the transcript, got:\n%s", out.String())
	}
	// v2 lipgloss colors the `$` glyph even when writing to a buffer (no TTY), so strip
	// ANSI before matching the prompt line.
	clean := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(out.String(), "")
	if !strings.Contains(clean, "$ echo hello-from-shell") {
		t.Errorf("the `$` prompt line should render, got:\n%s", out.String())
	}

	// High-fidelity output: a command whose output contains a BLANK line must keep it
	// verbatim (the bug was the markdown path eating blanks + branding words).
	out.Reset()
	s.Submit(ctx, chat, "$ printf 'first\\n\\nlast\\n'")
	if !strings.Contains(out.String(), "first\n\nlast") {
		t.Errorf("blank lines in shell output must be preserved verbatim, got:\n%q", out.String())
	}

	// A git command via `$` is captured so the accountability trail includes it,
	// even though it fails in this non-repo temp dir (we only care it's recorded).
	s.Submit(ctx, chat, `$ git commit -m "manual via shell"`)
	s.EndChat(ctx)

	recs, err := sessionlog.Recent(root, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawEcho bool
	for _, r := range recs {
		if r.Kind == sessionlog.KindToolCall && strings.Contains(r.Input, "echo hello-from-shell") {
			sawEcho = true
		}
	}
	if !sawEcho {
		t.Error("the `$ echo` command should be recorded in the episodic log as a tool call")
	}
	if commits, _ := sessionlog.Commits(root); len(commits) == 0 {
		t.Error("a `$ git commit` should surface in the commits trail")
	}
}

// TestExplainFixLast is the explain/fix-last loop: a failed `$` command must be
// recallable — command, output, and non-zero exit — so "why did that fail?" / "fix
// it" can consult it.
func TestExplainFixLast(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := newSess(st, &scriptProvider{}, root, "sonnet", permissions.ModeAsk, &bytes.Buffer{})
	s.SetApprover(func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true}
	})
	chat := s.StartChat(ctx)

	// A command that fails with a distinctive message on stderr and a non-zero exit.
	s.Submit(ctx, chat, "$ sh -c 'echo boom-on-stderr 1>&2; exit 3'")

	r, ok := sessionlog.LastShell(root)
	if !ok {
		t.Fatal("LastShell should return the `$` command just run")
	}
	if !strings.Contains(r.Input, "boom-on-stderr") {
		t.Errorf("recalled command wrong: %q", r.Input)
	}
	if !r.IsError || r.Exit != 3 {
		t.Errorf("failed command should record IsError + exit 3, got IsError=%v exit=%d", r.IsError, r.Exit)
	}
	if !strings.Contains(r.Content, "boom-on-stderr") {
		t.Errorf("the output (incl. stderr) should be captured for explain/fix, got: %q", r.Content)
	}

	// The agent's recall tool renders it for explain/fix-last.
	out, isErr := s.Intelligence(ctx, "session", "shell")
	if isErr || !strings.Contains(out, "boom-on-stderr") || !strings.Contains(out, "exit 3") {
		t.Errorf("`session shell` should render the last command + output + exit, got:\n%s", out)
	}
}

// TestRunShellLineImmediate locks the fix for shell-lane commands queuing behind a turn: the `$`
// lane runs IMMEDIATELY via RunShellLine (the front-end calls this instead of the scheduler), and
// a non-shell line is reported as not-a-shell-route so the caller queues/runs it normally.
func TestRunShellLineImmediate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var out bytes.Buffer
	s := newSess(st, &scriptProvider{}, root, "sonnet", permissions.ModeAsk, &out)
	s.SetApprover(func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true}
	})
	s.StartChat(ctx)

	if ran := s.RunShellLine(ctx, "$ echo immediate-shell"); !ran {
		t.Fatal("RunShellLine should report a shell route for a `$` line")
	}
	if !strings.Contains(out.String(), "immediate-shell") {
		t.Errorf("the shell command should have executed immediately, got:\n%s", out.String())
	}

	out.Reset()
	if ran := s.RunShellLine(ctx, "build me a feature"); ran {
		t.Error("a non-shell line must report false (the caller routes it through the scheduler)")
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("a non-shell line must execute nothing via RunShellLine, got:\n%s", out.String())
	}
}
