package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
)

// TestChatReadOnlyDeniesMutation locks the hard "nothing may mutate" guarantee of the
// Shift+Tab / `/mode` "Read Only" stop: every mutating tool is denied at EXECUTION time
// (not just hidden from advertisement), while investigative tools stay fully available.
func TestChatReadOnlyDeniesMutation(t *testing.T) {
	s := newTodoSession(t)
	s.chatReadOnly = true

	// EditFile / ApplyPatch / Todo / MCPCodeExec: denied by the dispatch-level mutating-tool
	// block (isMutatingTool), the same gate readOnly/plan mode go through.
	for _, name := range []string{tools.EditFile, tools.ApplyPatch, tools.Todo, tools.MCPCodeExec} {
		r := s.dispatch(context.Background(), tu("x", name, `{}`))
		if !r.isError || !strings.Contains(r.text(), "Read Only") {
			t.Errorf("%s: expected a Read Only denial, got isErr=%v text=%q", name, r.isError, r.text())
		}
	}

	// Bash: a mutating command is denied by bash()'s own research-context check; a Safe
	// (read-only) command still runs — Read Only blocks MUTATION, not reading.
	r := s.dispatch(context.Background(), tu("x", tools.Bash, `{"command":"rm -rf /tmp/whatever"}`))
	if !r.isError || !strings.Contains(r.text(), "Read Only") {
		t.Errorf("bash(rm): expected a Read Only denial, got isErr=%v text=%q", r.isError, r.text())
	}
	r = s.dispatch(context.Background(), tu("x", tools.Bash, `{"command":"echo hi"}`))
	if r.isError {
		t.Errorf("bash(echo): a Safe read-only command should still run under chatReadOnly, got error %q", r.text())
	}

	// Artifact publish/update/delete: denied by artifactTool's own guard; list stays available
	// (checked separately — list needs no account/network here, so just confirm it's not
	// rejected by the SAME Read Only guard as publish/update/delete).
	for _, action := range []string{"publish", "update", "delete"} {
		r := s.artifactTool(context.Background(), []byte(`{"action":"`+action+`","path":"x.html","title":"x","id":"1"}`))
		if !r.isError || !strings.Contains(r.text(), "read-only") {
			t.Errorf("artifact %s: expected a read-only denial, got isErr=%v text=%q", action, r.isError, r.text())
		}
	}

	// Dispatch / Agent: hidden from advertisement under Read Only (same parity as readOnly/plan
	// mode — neither has a defensive execution-time check either; hiding is the enforcement).
	for _, name := range []string{tools.Dispatch, tools.Agent} {
		if s.allowTool(name) {
			t.Errorf("%s must not be offered under chatReadOnly", name)
		}
	}

	// Investigative tools stay fully available: Read Only keeps full investigative capability.
	for _, name := range []string{tools.WebSearch, tools.Fetch, tools.AskUser} {
		if !s.allowTool(name) {
			t.Errorf("%s must remain available under chatReadOnly", name)
		}
	}
	if !s.allowTool(tools.EnterPlan) {
		t.Error("enter_plan must remain available under chatReadOnly (/plan is the way out)")
	}
}

// TestExitReadOnlyForExecution locks the ONE centralized choke point that lifts Read Only:
// it atomically clears chatReadOnly and sets the mode in the same call, and is a no-op
// (returns false, changes nothing) when the session wasn't read-only to begin with.
func TestExitReadOnlyForExecution(t *testing.T) {
	s := newTodoSession(t)
	s.chatReadOnly = true

	if ok := s.ExitReadOnlyForExecution(permissions.ModeAuto); !ok {
		t.Fatal("expected ExitReadOnlyForExecution to report a transition (chatReadOnly was true)")
	}
	if s.chatReadOnly {
		t.Error("chatReadOnly should be false after ExitReadOnlyForExecution")
	}
	if s.Mode() != permissions.ModeAuto {
		t.Errorf("Mode() = %v, want ModeAuto", s.Mode())
	}

	// Second call: already not read-only — a no-op, and it must NOT clobber the mode just set.
	if ok := s.ExitReadOnlyForExecution(permissions.ModeAsk); ok {
		t.Error("a second call (already not read-only) must be a no-op (return false)")
	}
	if s.Mode() != permissions.ModeAuto {
		t.Errorf("a no-op call must not change the mode: Mode() = %v, want ModeAuto", s.Mode())
	}
}
