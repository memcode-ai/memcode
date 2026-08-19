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
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// hooksSess builds a session whose project hooks.json is cfg. HOME is pointed
// at an empty tempdir so the developer's own ~/.memcode/hooks.json can't leak in.
func hooksSess(t *testing.T, root, cfg string) *Session {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(root, ".memcode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".memcode", "hooks.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newSess(st, captureProviderNil{}, root, "allow-all", permissions.ModeAllowAll, io.Discard)
}

func toolUse(id, name string, input any) wire.Block {
	raw, _ := json.Marshal(input)
	return wire.Block{Type: "tool_use", ID: id, Name: name, Input: raw}
}

// TestPreToolHookVetoBlocksCall: a pre_tool_use hook exiting 2 must stop the
// call from executing and produce an IsError tool_result carrying the reason.
func TestPreToolHookVetoBlocksCall(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := hooksSess(t, root,
		`{"hooks":{"pre_tool_use":[{"matcher":"read_file","command":"echo 'not allowed' >&2; exit 2"}]}}`)

	results := s.executeBatchHooked(context.Background(),
		[]wire.Block{toolUse("t1", "read_file", map[string]string{"path": "f.txt"})})
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	r := results[0]
	if !r.IsError {
		t.Errorf("vetoed call's tool_result must be IsError: %+v", r)
	}
	if r.ToolUseID != "t1" {
		t.Errorf("tool_result id = %q, want t1", r.ToolUseID)
	}
	if !strings.Contains(r.Content, "not allowed") {
		t.Errorf("veto reason missing from tool_result: %q", r.Content)
	}
	if strings.Contains(r.Content, "hello") {
		t.Errorf("blocked call must not have executed: %q", r.Content)
	}
}

// TestPreToolHookVetoPreservesOrder: a batch mixing vetoed and executed calls
// must return tool_results in REQUEST order with matching ids — vetoed results
// were previously appended at the end.
func TestPreToolHookVetoPreservesOrder(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := hooksSess(t, root,
		`{"hooks":{"pre_tool_use":[{"matcher":"list_dir","command":"exit 2"}]}}`)

	uses := []wire.Block{
		toolUse("t1", "list_dir", map[string]string{"path": "."}),      // vetoed
		toolUse("t2", "read_file", map[string]string{"path": "f.txt"}), // runs
		toolUse("t3", "list_dir", map[string]string{"path": "."}),      // vetoed
	}
	results := s.executeBatchHooked(context.Background(), uses)
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	for i, want := range []string{"t1", "t2", "t3"} {
		if results[i].ToolUseID != want {
			t.Fatalf("result %d id = %q, want %q (order not preserved)", i, results[i].ToolUseID, want)
		}
	}
	if !results[0].IsError || !results[2].IsError {
		t.Errorf("vetoed calls must be IsError: %+v", results)
	}
	if results[1].IsError {
		t.Errorf("allowed call should have executed cleanly: %+v", results[1])
	}
}
