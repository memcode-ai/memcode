package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/plan"
	"github.com/memcode-ai/memcode/internal/sandbox"
	"github.com/memcode-ai/memcode/internal/store"
)

func codeExecTestSession(t *testing.T, mode permissions.Mode) *Session {
	t.Helper()
	if stdruntime.GOOS == "windows" {
		t.Skip("mcp_code_exec is not available on Windows")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Session{
		root:    dir,
		store:   st,
		out:     io.Discard,
		mode:    mode,
		turn:    newTurnState(),
		planCtl: &plan.Controller{},
	}
}

// codeExecMCPSession is codeExecTestSession plus the in-binary echo MCP server
// wired in — the bridge surface is MCP-only, so every bridge test needs it.
func codeExecMCPSession(t *testing.T, mode permissions.Mode) *Session {
	t.Helper()
	s := codeExecTestSession(t, mode)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	m := connectEchoMCP(t, ctx)
	t.Cleanup(func() { m.Close() })
	s.mcp = m
	return s
}

func codeExecInput(t *testing.T, in map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCodeExecDistillsIntermediates is the core contract: the script makes many
// MCP calls through the bridge and only its small printed result returns — the
// intermediates stay script-side. The asserted byte ratio is the token-savings
// mechanism made measurable.
func TestCodeExecDistillsIntermediates(t *testing.T) {
	s := codeExecMCPSession(t, permissions.ModeAllowAll)
	script := `
payload = "x" * 2000
total = 0
for i in range(20):
    r = mcp("mcp__fake__echo", text=payload)
    total += len(r)
print(f"calls=20 bytes={total}")
`
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{"script": script}))
	if tr.isError {
		t.Fatalf("mcp_code_exec failed: %s", tr.text())
	}
	out := tr.text()
	if !strings.Contains(out, "calls=20") {
		t.Fatalf("distilled result missing: %q", out)
	}
	if len(out) > 512 {
		t.Errorf("result should be distilled (<512B), got %dB", len(out))
	}
}

// TestCodeExecReadProxyNudge: a one-call script that prints ~everything it
// bridged is a plain read wearing a subprocess — the result carries an advisory
// nudge to call the tool directly. A genuinely distilling script does not.
// (The measured audit session used mcp_code_exec as a pure read proxy 35/35 times.)
func TestCodeExecReadProxyNudge(t *testing.T) {
	s := codeExecMCPSession(t, permissions.ModeAllowAll)

	// Pure proxy: one MCP call, prints it all -> nudged.
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{
		"script": `print(mcp("mcp__fake__echo", text="p" * 10000))`,
	}))
	if tr.isError {
		t.Fatalf("mcp_code_exec failed: %s", tr.text())
	}
	if !strings.Contains(tr.text(), "call that tool directly") {
		t.Fatalf("pure proxy should carry the advisory nudge, got tail: %q", tail(tr.text(), 200))
	}

	// Distilling script: same bridged call, tiny result -> no nudge.
	tr = s.codeExec(context.Background(), codeExecInput(t, map[string]any{
		"script": `print(len(mcp("mcp__fake__echo", text="p" * 10000)))`,
	}))
	if tr.isError {
		t.Fatalf("mcp_code_exec failed: %s", tr.text())
	}
	if strings.Contains(tr.text(), "call that tool directly") {
		t.Fatalf("a distilling script must not be nudged: %q", tr.text())
	}
}

// TestCodeExecBridgedBytesAccounting proves the savings are visible: the run
// reports how many bytes crossed the bridge vs what the model received.
func TestCodeExecBridgedBytesAccounting(t *testing.T) {
	s := codeExecMCPSession(t, permissions.ModeAllowAll)
	python, _ := exec.LookPath("python3")
	res := s.runBridgedScript(context.Background(), python,
		`a = mcp("mcp__fake__echo", text="n" * 30000)`+"\n"+
			`b = mcp("mcp__fake__echo", text="n" * 20000)`+"\n"+
			`print("lens:", len(a), len(b))`)
	if res.err != nil {
		t.Fatalf("run failed: %v (stderr: %s)", res.err, res.stderr)
	}
	if res.calls != 2 {
		t.Errorf("expected 2 bridged calls, got %d", res.calls)
	}
	if res.bridgedBytes < 50_000 {
		t.Errorf("expected >50KB bridged, got %d", res.bridgedBytes)
	}
	if len(res.stdout) > 100 {
		t.Errorf("distilled stdout should be tiny, got %dB", len(res.stdout))
	}
	ratio := float64(len(res.stdout)) / float64(res.bridgedBytes)
	t.Logf("token-delta proof: %dB bridged -> %dB returned (%.1f%% of the intermediate volume)",
		res.bridgedBytes, len(res.stdout), ratio*100)
}

// TestCodeExecGatherParallel: gather() fans several bridge calls out
// concurrently and returns results in request order even when replies arrive
// out of order — the PTC orchestration primitive.
func TestCodeExecGatherParallel(t *testing.T) {
	s := codeExecMCPSession(t, permissions.ModeAllowAll)
	script := `
results = gather(*[("mcp__fake__echo", {"text": "y" * ((i + 1) * 100)}) for i in range(6)])
print(",".join(str(len(r) - len("echo: ")) for r in results))
`
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{"script": script}))
	if tr.isError {
		t.Fatalf("gather failed: %s", tr.text())
	}
	if !strings.HasPrefix(tr.text(), "100,200,300,400,500,600") {
		t.Errorf("gather results must map back in request order, got: %s", tr.text())
	}
}

// TestCodeExecPersistentWorkspace: a script's own file I/O lands in
// .memcode/codeexec/ and survives to the next run (state-persistence pattern).
func TestCodeExecPersistentWorkspace(t *testing.T) {
	s := codeExecTestSession(t, permissions.ModeAllowAll)
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{
		"script": "open(\"stash.txt\", \"w\").write(\"kept\")\nprint(\"wrote\")",
	}))
	if tr.isError {
		t.Fatalf("first run failed: %s", tr.text())
	}
	if _, err := os.Stat(filepath.Join(s.root, ".memcode", "codeexec", "stash.txt")); err != nil {
		t.Fatalf("workspace file not persisted: %v", err)
	}
	tr = s.codeExec(context.Background(), codeExecInput(t, map[string]any{
		"script": "print(open(\"stash.txt\").read())",
	}))
	if tr.isError || !strings.Contains(tr.text(), "kept") {
		t.Errorf("second run should read the stash, got error=%v text=%s", tr.isError, tr.text())
	}
}

// TestCodeExecWhitelistOnly: the bridge surface is MCP-ONLY. Mutating tools AND
// the standard read-only repo tools (read_file, ripgrep, ...) are refused — the
// repo tools live on the direct path; bridging them turned mcp_code_exec into a
// read proxy (35/35 calls in the audited session).
func TestCodeExecWhitelistOnly(t *testing.T) {
	s := codeExecTestSession(t, permissions.ModeAllowAll)
	for _, script := range []string{
		`call("bash", command="echo pwned")`,
		`call("read_file", path="go.mod")`,
		`call("ripgrep", query="needle")`,
	} {
		tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{"script": script}))
		if !tr.isError {
			t.Fatalf("expected error for %s, got: %s", script, tr.text())
		}
		if !strings.Contains(tr.text(), "not available in mcp_code_exec") {
			t.Errorf("error should name the refusal for %s, got: %s", script, tr.text())
		}
	}
}

// TestCodeExecNoNetwork: the sandbox really cuts egress — a script that
// connects to a LIVE local listener must fail. This is the containment the
// Medium gating rests on; if this test fails, the risk downgrade is unsound.
func TestCodeExecNoNetwork(t *testing.T) {
	s := codeExecTestSession(t, permissions.ModeAllowAll)
	if !sandbox.Supported(codeExecPolicy(false, s.root)) {
		t.Skip("no platform sandbox — containment (and the Medium gate) doesn't apply here")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	script := `
import socket
try:
    s = socket.create_connection(("127.0.0.1", ` + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + `), timeout=3)
    s.close()
    print("CONNECTED")
except Exception as e:
    print("BLOCKED:", type(e).__name__)
`
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{"script": script}))
	if tr.isError {
		t.Fatalf("script failed to run: %s", tr.text())
	}
	if !strings.Contains(tr.text(), "BLOCKED") {
		t.Fatalf("sandbox must deny network egress, got: %s", tr.text())
	}
}

// TestCodeExecGateScalesToContainment: sandboxed runs gate at Medium (auto-run
// in auto mode, NO prompt); with the sandbox off the same call gates at
// Dangerous (prompts in auto). Containment replaces confirmation.
func TestCodeExecGateScalesToContainment(t *testing.T) {
	s := codeExecTestSession(t, permissions.ModeAuto)
	if !sandbox.Supported(codeExecPolicy(false, s.root)) {
		t.Skip("no platform sandbox")
	}
	prompted := false
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		prompted = true
		return Denied("no")
	}
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{"script": `print("ok")`}))
	if tr.isError {
		t.Fatalf("sandboxed run in auto mode should auto-run: %s", tr.text())
	}
	if prompted {
		t.Error("sandboxed run must NOT prompt in auto mode (Medium)")
	}

	t.Setenv(sandbox.EnvVar, "0") // explicit sandbox off → uncontained → Dangerous
	tr = s.codeExec(context.Background(), codeExecInput(t, map[string]any{"script": `print("ok")`}))
	if !prompted {
		t.Error("unsandboxed run must prompt in auto mode (Dangerous)")
	}
	if !tr.isError || !strings.Contains(tr.text(), "denied") {
		t.Errorf("denial should stop the run, got error=%v text=%s", tr.isError, tr.text())
	}
}

// TestCodeExecGateDenied: in ask mode a denial short-circuits before anything runs.
func TestCodeExecGateDenied(t *testing.T) {
	s := codeExecTestSession(t, permissions.ModeAsk)
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		return Denied("not now")
	}
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{"script": `print("hi")`}))
	if !tr.isError || !strings.Contains(tr.text(), "denied") {
		t.Fatalf("expected gate denial, got error=%v text=%s", tr.isError, tr.text())
	}
}

// TestCodeExecCancellation: a hung script dies with the turn context, it does
// not hang the loop.
func TestCodeExecCancellation(t *testing.T) {
	s := codeExecTestSession(t, permissions.ModeAllowAll)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	tr := s.codeExec(ctx, codeExecInput(t, map[string]any{
		"script": "import time\ntime.sleep(60)\nprint('never')",
	}))
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("cancellation took %s — process group not killed", elapsed)
	}
	if !tr.isError {
		t.Fatalf("expected error on cancellation, got: %s", tr.text())
	}
}

// TestCodeExecEmptyOutputIsError: a script that prints nothing gets a corrective
// error, not a silent empty result.
func TestCodeExecEmptyOutputIsError(t *testing.T) {
	s := codeExecTestSession(t, permissions.ModeAllowAll)
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{"script": `x = 1 + 1`}))
	if !tr.isError || !strings.Contains(tr.text(), "printed nothing") {
		t.Fatalf("expected printed-nothing error, got error=%v text=%s", tr.isError, tr.text())
	}
}

// TestCodeExecScriptErrorSurfacesStderr: a traceback comes back to the model so
// it can fix the script.
func TestCodeExecScriptErrorSurfacesStderr(t *testing.T) {
	s := codeExecTestSession(t, permissions.ModeAllowAll)
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{
		"script": `raise ValueError("boom-marker")`,
	}))
	if !tr.isError {
		t.Fatal("expected error result")
	}
	if !strings.Contains(tr.text(), "boom-marker") {
		t.Errorf("traceback should surface, got: %s", tr.text())
	}
}

// TestCodeExecSaveSkill: a successful run with save_skill persists a valid
// SKILL.md under .memcode/skills/<slug>/ (Medium-gated, auto-runs in allow-all).
func TestCodeExecSaveSkill(t *testing.T) {
	s := codeExecTestSession(t, permissions.ModeAllowAll)
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{
		"script":                 `print("ok")`,
		"save_skill":             "count-needles",
		"save_skill_description": "Count needle lines in a haystack file",
	}))
	if tr.isError {
		t.Fatalf("mcp_code_exec failed: %s", tr.text())
	}
	if !strings.Contains(tr.text(), "saved as skill `count-needles`") {
		t.Errorf("result should note the save, got: %s", tr.text())
	}
	body, err := os.ReadFile(filepath.Join(s.root, ".memcode", "skills", "count-needles", "SKILL.md"))
	if err != nil {
		t.Fatalf("SKILL.md not written: %v", err)
	}
	for _, want := range []string{"name: count-needles", "description: Count needle", "```python", `print("ok")`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("SKILL.md missing %q", want)
		}
	}
}

// TestCodeExecSaveSkillRejectsBadSlug: slug validation refuses non-slug names
// without failing the run itself.
func TestCodeExecSaveSkillRejectsBadSlug(t *testing.T) {
	s := codeExecTestSession(t, permissions.ModeAllowAll)
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{
		"script":                 `print("ok")`,
		"save_skill":             "Bad_Name",
		"save_skill_description": "x",
	}))
	if tr.isError {
		t.Fatalf("run itself should succeed: %s", tr.text())
	}
	if !strings.Contains(tr.text(), "lowercase-hyphen") {
		t.Errorf("expected slug rejection note, got: %s", tr.text())
	}
	if _, err := os.Stat(filepath.Join(s.root, ".memcode", "skills", "Bad_Name")); !os.IsNotExist(err) {
		t.Error("bad-slug skill dir must not be created")
	}
}

// TestCodeExecNotParallelSafeAndMutating: registry posture — arbitrary code is
// serial and withheld from read-only research modes.
func TestCodeExecNotParallelSafeAndMutating(t *testing.T) {
	if isParallelSafe("mcp_code_exec") {
		t.Error("mcp_code_exec must not be parallel-safe")
	}
	if !isMutatingTool("mcp_code_exec") {
		t.Error("mcp_code_exec must be treated as mutating (withheld read-only/plan)")
	}
}

// The runaway-loop backstop: calls past maxBridgeCalls get error replies (the script keeps
// running — each refused call raises, so a well-written script can still print what it has).
func TestCodeExecBridgeCallCap(t *testing.T) {
	s := codeExecMCPSession(t, permissions.ModeAllowAll)
	script := `
errs = 0
for i in range(205):
    try:
        mcp("mcp__fake__echo", text="hi")
    except Exception:
        errs += 1
print("refused", errs)
`
	tr := s.codeExec(context.Background(), codeExecInput(t, map[string]any{"script": script}))
	if tr.isError {
		t.Fatalf("mcp_code_exec failed: %s", tr.text())
	}
	if out := tr.text(); !strings.Contains(out, "refused 5") {
		t.Fatalf("calls 201..205 should be refused: %q", out)
	}
}
