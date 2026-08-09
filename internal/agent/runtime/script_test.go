package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/scripts"
)

func scriptSession(t *testing.T) *Session {
	t.Helper()
	return newTodoSession(t)
}

func (s *Session) script(t *testing.T, input string) toolResult {
	t.Helper()
	return s.useScript(context.Background(), []byte(input))
}

// TestScriptSaveGated: saving goes through the standard approval card, and only lands on
// disk once approved.
func TestScriptSaveGated(t *testing.T) {
	s := scriptSession(t)
	asked := 0
	s.approve = func(_ context.Context, r ApprovalRequest) ApprovalDecision {
		asked++
		if r.Title != "rebuild-cli" || r.Label != "Save script" {
			t.Errorf("unexpected approval request: %+v", r)
		}
		return ApprovalDecision{Allow: true}
	}
	r := s.script(t, `{"save":"rebuild-cli","description":"Rebuild the CLI binary","command":"cd cli && go build ./..."}`)
	if r.isError {
		t.Fatalf("expected save to succeed: %q", r.text())
	}
	if asked != 1 {
		t.Errorf("save must be gated by exactly one ask, got %d", asked)
	}
	sc, ok := scripts.Get(s.root, "rebuild-cli")
	if !ok {
		t.Fatal("expected the script to be saved")
	}
	if sc.Body != "cd cli && go build ./..." {
		t.Errorf("body = %q", sc.Body)
	}
}

// TestScriptSaveDeclined: a declined save writes nothing.
func TestScriptSaveDeclined(t *testing.T) {
	s := scriptSession(t)
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision { return ApprovalDecision{} }
	r := s.script(t, `{"save":"rebuild-cli","description":"desc","command":"echo hi"}`)
	if !r.isError || !strings.Contains(r.text(), "not saved") {
		t.Errorf("declined save should report not-saved: %q (err=%v)", r.text(), r.isError)
	}
	if _, ok := scripts.Get(s.root, "rebuild-cli"); ok {
		t.Error("declined save must not write the script")
	}
}

// TestScriptSaveInvalidInput: bad slug/empty description/empty command are clean errors, not
// prompts.
func TestScriptSaveInvalidInput(t *testing.T) {
	s := scriptSession(t)
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		t.Error("invalid input must not reach the approval gate")
		return ApprovalDecision{}
	}
	if r := s.script(t, `{"save":"Bad Slug","description":"d","command":"echo hi"}`); !r.isError {
		t.Error("expected error for invalid slug")
	}
	if r := s.script(t, `{"save":"ok-slug","description":"","command":"echo hi"}`); !r.isError {
		t.Error("expected error for empty description")
	}
	if r := s.script(t, `{"save":"ok-slug","description":"d","command":""}`); !r.isError {
		t.Error("expected error for empty command")
	}
}

// TestScriptDeleteGated: delete is gated the same way as save, and soft-deletes to .trash.
func TestScriptDeleteGated(t *testing.T) {
	s := scriptSession(t)
	if _, err := scripts.Save(s.root, "throwaway", "desc", "echo bye"); err != nil {
		t.Fatal(err)
	}
	asked := 0
	s.approve = func(_ context.Context, r ApprovalRequest) ApprovalDecision {
		asked++
		if r.Title != "throwaway" || r.Label != "Delete script" {
			t.Errorf("unexpected approval request: %+v", r)
		}
		return ApprovalDecision{Allow: true}
	}
	r := s.script(t, `{"delete":"throwaway"}`)
	if r.isError {
		t.Fatalf("expected delete to succeed: %q", r.text())
	}
	if asked != 1 {
		t.Errorf("delete must be gated by exactly one ask, got %d", asked)
	}
	if _, ok := scripts.Get(s.root, "throwaway"); ok {
		t.Error("expected the script to be gone from the live store")
	}
	entries, err := os.ReadDir(filepath.Join(scripts.Dir(s.root), ".trash"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one trashed file, got %v (err %v)", entries, err)
	}
}

// TestScriptDeleteDeclined: a declined delete leaves the script in place.
func TestScriptDeleteDeclined(t *testing.T) {
	s := scriptSession(t)
	if _, err := scripts.Save(s.root, "throwaway", "desc", "echo bye"); err != nil {
		t.Fatal(err)
	}
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision { return ApprovalDecision{} }
	r := s.script(t, `{"delete":"throwaway"}`)
	if !r.isError {
		t.Fatal("expected decline to error")
	}
	if _, ok := scripts.Get(s.root, "throwaway"); !ok {
		t.Error("declined delete must not remove the script")
	}
}

// TestScriptDeleteUnknown: deleting a slug that was never saved is a clean error, not a crash.
func TestScriptDeleteUnknown(t *testing.T) {
	s := scriptSession(t)
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		t.Error("must not prompt for a slug that doesn't exist")
		return ApprovalDecision{}
	}
	r := s.script(t, `{"delete":"does-not-exist"}`)
	if !r.isError || !strings.Contains(r.text(), "no saved script") {
		t.Errorf("unknown delete should error cleanly: %q", r.text())
	}
}

// TestScriptListAndFindUngated: list/find are read-only discovery — never gated.
func TestScriptListAndFindUngated(t *testing.T) {
	s := scriptSession(t)
	if _, err := scripts.Save(s.root, "rebuild-cli", "Rebuild the CLI binary", "go build ./..."); err != nil {
		t.Fatal(err)
	}
	if _, err := scripts.Save(s.root, "commit-push-deploy", "Commit, push, and deploy", "git push && deploy"); err != nil {
		t.Fatal(err)
	}
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		t.Error("list/find must not prompt for approval")
		return ApprovalDecision{}
	}

	if r := s.script(t, `{"list":true}`); r.isError || !strings.Contains(r.text(), "rebuild-cli") || !strings.Contains(r.text(), "commit-push-deploy") {
		t.Errorf("list should show both scripts: %q (err=%v)", r.text(), r.isError)
	}
	if r := s.script(t, `{"find":"deploy"}`); r.isError || !strings.Contains(r.text(), "commit-push-deploy") {
		t.Errorf("find should match commit-push-deploy: %q (err=%v)", r.text(), r.isError)
	}
	if r := s.script(t, `{"find":"nothing-like-this"}`); r.isError {
		t.Errorf("a find miss should be a plain text result, not an error: %q", r.text())
	}
}

// TestScriptListEmpty: an empty store is a friendly nudge, not an error.
func TestScriptListEmpty(t *testing.T) {
	s := scriptSession(t)
	r := s.script(t, `{"list":true}`)
	if r.isError || !strings.Contains(r.text(), "no saved scripts") {
		t.Errorf("empty list should say so plainly: %q (err=%v)", r.text(), r.isError)
	}
}

// TestScriptRunUnknownSlug: running a slug that was never saved is a clean, helpful error.
func TestScriptRunUnknownSlug(t *testing.T) {
	s := scriptSession(t)
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		t.Error("an unknown slug must not reach the bash approval gate")
		return ApprovalDecision{}
	}
	r := s.script(t, `{"run":"does-not-exist"}`)
	if !r.isError || !strings.Contains(r.text(), "no saved script") {
		t.Errorf("unknown run should error cleanly: %q", r.text())
	}
}

// TestScriptRunUnknownSlugSuggestsNearMatch: a near-miss slug gets a "did you mean" hint.
func TestScriptRunUnknownSlugSuggestsNearMatch(t *testing.T) {
	s := scriptSession(t)
	if _, err := scripts.Save(s.root, "rebuild-cli", "Rebuild the CLI binary", "go build ./..."); err != nil {
		t.Fatal(err)
	}
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision { return ApprovalDecision{} }
	r := s.script(t, `{"run":"rebuild-cl"}`)
	if !r.isError || !strings.Contains(r.text(), "rebuild-cli") {
		t.Errorf("expected a near-match suggestion: %q", r.text())
	}
}

// TestScriptRunGatedOnce: `run` on a KNOWN slug is gated exactly ONCE, at the script level
// ("run script X?") — NOT re-classified/re-approved command-by-command the way a raw bash
// call would be. A decline reports a clean error and never executes the command.
func TestScriptRunGatedOnce(t *testing.T) {
	s := scriptSession(t)
	dir := t.TempDir()
	if _, err := scripts.Save(s.root, "make-a-file", "touch a new file", "touch "+filepath.Join(dir, "out.txt")); err != nil {
		t.Fatal(err)
	}
	asked := 0
	s.approve = func(_ context.Context, r ApprovalRequest) ApprovalDecision {
		asked++
		if r.Title != "make-a-file" || r.Label != "Run script" {
			t.Errorf("unexpected approval request: %+v", r)
		}
		return Denied("not now")
	}
	r := s.script(t, `{"run":"make-a-file"}`)
	if !r.isError || !strings.Contains(r.text(), "not run") {
		t.Errorf("declined run should report not-run: %q", r.text())
	}
	if asked != 1 {
		t.Errorf("run must go through exactly one script-level ask, got %d", asked)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Error("declined run must not actually execute the command")
	}
	sc, _ := scripts.Get(s.root, "make-a-file")
	if sc.RunCount != 0 {
		t.Errorf("a declined run must not bump the run count: %+v", sc)
	}
}

// TestScriptRunApprovedExecutesAndRecordsRun: an approved run actually executes the saved
// body and bumps the run counter — WITHOUT the classifier ever weighing in a second time.
func TestScriptRunApprovedExecutesAndRecordsRun(t *testing.T) {
	s := scriptSession(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")
	if _, err := scripts.Save(s.root, "make-a-file", "touch a new file", "touch "+target); err != nil {
		t.Fatal(err)
	}
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision { return ApprovalDecision{Allow: true} }
	r := s.script(t, `{"run":"make-a-file"}`)
	if r.isError {
		t.Fatalf("expected the run to succeed: %q", r.text())
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("expected the command to actually run: %v", err)
	}
	sc, ok := scripts.Get(s.root, "make-a-file")
	if !ok || sc.RunCount != 1 {
		t.Fatalf("expected RecordRun to bump the run count after a successful run: %+v", sc)
	}
}

// TestScriptRunRememberSkipsReask: "don't ask again" (the same session-wide edits-remember
// as save/delete) suppresses the ask on a later run — of the SAME or a DIFFERENT script,
// since it's one coarse "stop asking for edits/scripts this session" grant, not a per-slug rule.
func TestScriptRunRememberSkipsReask(t *testing.T) {
	s := scriptSession(t)
	dir := t.TempDir()
	if _, err := scripts.Save(s.root, "one", "first script", "touch "+filepath.Join(dir, "one.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := scripts.Save(s.root, "two", "second script", "touch "+filepath.Join(dir, "two.txt")); err != nil {
		t.Fatal(err)
	}
	asked := 0
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		asked++
		return ApprovalDecision{Allow: true, Remember: true}
	}
	if r := s.script(t, `{"run":"one"}`); r.isError {
		t.Fatalf("first run failed: %q", r.text())
	}
	if r := s.script(t, `{"run":"two"}`); r.isError {
		t.Fatalf("second run failed: %q", r.text())
	}
	if asked != 1 {
		t.Errorf("remember should suppress re-asks across scripts this session: asked %d times, want 1", asked)
	}
}

// TestScriptRunBackgroundStartsDetached: run:background hands Background through to the
// execution path unchanged, same as a live-typed background bash command.
func TestScriptRunBackgroundStartsDetached(t *testing.T) {
	s := scriptSession(t)
	if _, err := scripts.Save(s.root, "watcher", "a long-running watcher", "sleep 5"); err != nil {
		t.Fatal(err)
	}
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision { return ApprovalDecision{Allow: true} }
	r := s.script(t, `{"run":"watcher","background":true}`)
	if r.isError {
		t.Fatalf("expected the background run to start cleanly: %q", r.text())
	}
	if !strings.Contains(r.text(), "started shell") {
		t.Errorf("expected the background-shell hand-off message: %q", r.text())
	}
}
