package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeHooks(t *testing.T, dir string, cfg string) string {
	t.Helper()
	root := filepath.Join(dir, "project")
	if err := os.MkdirAll(filepath.Join(root, ".memcode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".memcode", "hooks.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook commands in these tests are sh scripts")
	}
}

func TestLoadEmptyAndMalformed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := Load(t.TempDir()) // no files at all
	if !s.Empty() {
		t.Fatal("no config must yield an empty set")
	}
	root := writeHooks(t, t.TempDir(), `{not json`)
	s = Load(root)
	if !s.Empty() || len(s.Warnings()) != 1 {
		t.Fatalf("malformed json must warn, not fail: empty=%v warnings=%v", s.Empty(), s.Warnings())
	}
	// Bad matcher → warning, hook skipped; good hook still loads.
	root = writeHooks(t, t.TempDir(),
		`{"hooks":{"pre_tool_use":[{"matcher":"([","command":"true"},{"command":"true"}]}}`)
	s = Load(root)
	if s.Empty() || len(s.Warnings()) != 1 {
		t.Fatalf("bad matcher: empty=%v warnings=%v", s.Empty(), s.Warnings())
	}
}

func TestPreToolUseBlockAndMatcher(t *testing.T) {
	skipOnWindows(t)
	t.Setenv("HOME", t.TempDir())
	root := writeHooks(t, t.TempDir(), `{"hooks":{"pre_tool_use":[
		{"matcher":"bash","command":"echo 'no shells today' >&2; exit 2"},
		{"command":"exit 0"}
	]}}`)
	s := Load(root)

	res := s.Run(context.Background(), PreToolUse, "bash", map[string]any{"tool": "bash"})
	if len(res) != 2 {
		t.Fatalf("want both hooks to run for bash, got %d results", len(res))
	}
	if !res[0].Block || !strings.Contains(res[0].Message, "no shells today") {
		t.Fatalf("exit 2 must block with stderr as reason: %+v", res[0])
	}
	if res[1].Block {
		t.Fatal("exit 0 must not block")
	}

	// Matcher filters: edit_file only hits the match-all hook.
	res = s.Run(context.Background(), PreToolUse, "edit_file", nil)
	if len(res) != 1 || res[0].Block {
		t.Fatalf("matcher must filter by tool name: %+v", res)
	}
}

func TestPayloadStdinAndEnvAndStdout(t *testing.T) {
	skipOnWindows(t)
	t.Setenv("HOME", t.TempDir())
	root := writeHooks(t, t.TempDir(),
		`{"hooks":{"session_start":[{"command":"cat; printf '|%s|%s' \"$MEMCODE_HOOK_EVENT\" \"$MEMCODE_PROJECT_DIR\""}]}}`)
	s := Load(root)
	payload := map[string]any{"event": SessionStart, "session_id": "sess_x"}
	res := s.Run(context.Background(), SessionStart, "", payload)
	if len(res) != 1 {
		t.Fatalf("got %d results", len(res))
	}
	var echoed map[string]any
	jsonPart := res[0].Stdout[:strings.Index(res[0].Stdout, "|")]
	if err := json.Unmarshal([]byte(jsonPart), &echoed); err != nil || echoed["session_id"] != "sess_x" {
		t.Fatalf("payload must arrive on stdin: %q (%v)", res[0].Stdout, err)
	}
	if !strings.Contains(res[0].Stdout, "|"+SessionStart+"|"+root) {
		t.Fatalf("env vars missing: %q", res[0].Stdout)
	}
}

// MEMCODE_SESSION_ID was promised by the package doc but never set (the env had
// only event/tool/project-dir). SetSessionID stamps the CURRENT id — re-stamped by
// the caller on every use, since /resume and /fork change it mid-Session.
func TestSessionIDEnv(t *testing.T) {
	skipOnWindows(t)
	t.Setenv("HOME", t.TempDir())
	root := writeHooks(t, t.TempDir(),
		`{"hooks":{"pre_tool_use":[{"command":"printf '%s' \"$MEMCODE_SESSION_ID\""}]}}`)
	s := Load(root)
	s.SetSessionID("sess_abc12345")
	res := s.Run(context.Background(), PreToolUse, "bash", nil)
	if len(res) != 1 || res[0].Stdout != "sess_abc12345" {
		t.Fatalf("MEMCODE_SESSION_ID must reach the hook env: %+v", res)
	}
	// Re-stamp (the /fork case): the NEW id must win.
	s.SetSessionID("sess_fork9999")
	res = s.Run(context.Background(), PreToolUse, "bash", nil)
	if len(res) != 1 || res[0].Stdout != "sess_fork9999" {
		t.Fatalf("a re-stamped session id must reach the hook env: %+v", res)
	}
}

func TestTimeoutAndNonBlockingFailure(t *testing.T) {
	skipOnWindows(t)
	t.Setenv("HOME", t.TempDir())
	root := writeHooks(t, t.TempDir(), `{"hooks":{"post_tool_use":[
		{"command":"sleep 5","timeout":1},
		{"command":"echo boom >&2; exit 1"}
	]}}`)
	s := Load(root)
	start := time.Now()
	res := s.Run(context.Background(), PostToolUse, "bash", nil)
	if time.Since(start) > 4*time.Second {
		t.Fatal("timeout did not bound the hook")
	}
	if len(res) != 2 {
		t.Fatalf("got %d results", len(res))
	}
	if res[0].Block || !strings.Contains(res[0].Message, "timed out") {
		t.Fatalf("timeout must warn, not block: %+v", res[0])
	}
	// exit 1 (not 2) and non-pre event: never a block, stderr surfaced.
	if res[1].Block || !strings.Contains(res[1].Message, "boom") {
		t.Fatalf("exit 1 must be a non-blocking warning: %+v", res[1])
	}
}

func TestUserAndProjectMerge(t *testing.T) {
	skipOnWindows(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".memcode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".memcode", "hooks.json"),
		[]byte(`{"hooks":{"session_end":[{"command":"echo user-hook"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	root := writeHooks(t, t.TempDir(), `{"hooks":{"session_end":[{"command":"echo project-hook"}]}}`)
	s := Load(root)
	res := s.Run(context.Background(), SessionEnd, "", nil)
	if len(res) != 2 || !strings.Contains(res[0].Stdout, "user-hook") || !strings.Contains(res[1].Stdout, "project-hook") {
		t.Fatalf("user hooks must run before project hooks: %+v", res)
	}
}
