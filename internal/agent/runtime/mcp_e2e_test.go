package runtime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/plan"
	"github.com/memcode-ai/memcode/internal/mcp"
)

// runEchoMCPServer is the in-binary stdio MCP server the session test dials (invoked from TestMain
// when MCP_TEST_SERVER=1). One "echo" tool that returns its `text` argument.
func runEchoMCPServer() {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "echo", Version: "v0"}, nil)
	srv.AddTool(&mcpsdk.Tool{
		Name:        "echo",
		Description: "echo the text back",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var a struct{ Text string }
		_ = json.Unmarshal(req.Params.Arguments, &a)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo: " + a.Text}}}, nil
	})
	_ = srv.Run(context.Background(), &mcpsdk.StdioTransport{})
}

// echoServerConfig is the stdio config that re-execs THIS test binary as the echo server.
// One definition so config-hash-keyed grant tests agree with what was connected.
func echoServerConfig() mcp.ServerConfig {
	return mcp.ServerConfig{Command: os.Args[0], Args: []string{"-test.run=TestNoSuchTest"}, Env: map[string]string{"MCP_TEST_SERVER": "1"}}
}

// connectEchoMCP dials THIS test binary as a stdio MCP server (TestMain re-routes on the env var).
func connectEchoMCP(t *testing.T, ctx context.Context) *mcp.Manager {
	t.Helper()
	m := mcp.Connect(ctx, map[string]mcp.ServerConfig{"fake": echoServerConfig()}, mcp.Options{Version: "test"})
	if errs := m.Errors(); len(errs) != 0 {
		t.Fatalf("mcp connect failed: %v", errs)
	}
	if !m.Has("mcp__fake__echo") {
		t.Fatalf("echo tool not discovered: %v", m.Tools())
	}
	return m
}

// The model can DISCOVER the MCP tools — by progressive disclosure only. toolDefs() carries
// the one constant `mcp` meta-tool (executive + plan, never read-only explorers) and ZERO
// per-tool schemas; the catalog is reached via search/schema, and the volatile facts carry
// the per-server index line.
func TestToolDefsSurfacesMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := connectEchoMCP(t, ctx)
	defer m.Close()

	hasMeta := func(s *Session) bool {
		found := false
		for _, d := range s.toolDefs() {
			if d.Name == "mcp" {
				found = true
			}
			if strings.HasPrefix(d.Name, "mcp__") {
				t.Errorf("per-tool MCP def leaked into the tools block: %s (schemas must be disclosed on demand)", d.Name)
			}
		}
		return found
	}

	exec := &Session{mcp: m, planCtl: &plan.Controller{}}
	if !hasMeta(exec) {
		t.Error("executive session must advertise the mcp meta-tool")
	}
	planning := &Session{mcp: m, planCtl: planCtlResearching()}
	if !hasMeta(planning) {
		t.Error("plan mode must advertise the mcp meta-tool")
	}
	explorer := &Session{mcp: m, planCtl: &plan.Controller{}, readOnly: true}
	if hasMeta(explorer) {
		t.Error("read-only explorer sub-agents must NOT receive the mcp meta-tool")
	}

	// Discovery surfaces: the facts index line, then search → schema on demand.
	if line := exec.mcpIndexFact(); !strings.Contains(line, "fake (1 tool)") {
		t.Errorf("facts index line should name the server and count: %q", line)
	}
	if out := exec.mcpSearch("echo"); !strings.Contains(out, "mcp__fake__echo") {
		t.Errorf("search should surface the tool: %q", out)
	}
	if out := exec.mcpSchema("mcp__fake__echo"); !strings.Contains(out, "input schema:") || !strings.Contains(out, "text") {
		t.Errorf("schema should return the tool's input schema: %q", out)
	}
}

// The model can CALL the MCP tools: a tool_use routes through invokeMCP → the real subprocess
// server and the result comes back as a non-error tool_result. And the gate is the USER's:
// allow-all auto-runs; ask/plan contexts prompt (no classifier hard-deny anymore) and the
// user's answer decides.
func TestInvokeMCPRoundTripAndGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := connectEchoMCP(t, ctx)
	defer m.Close()

	// Positive: allow-all executive → no prompt, the call reaches the server and echoes back.
	exec := &Session{mcp: m, planCtl: &plan.Controller{}, mode: permissions.ModeAllowAll, out: io.Discard}
	exec.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		t.Fatal("allow-all must not prompt for an MCP call")
		return Denied("")
	}
	r := exec.invokeMCP(ctx, mcpOriginDirect, "mcp__fake__echo", json.RawMessage(`{"text":"ping"}`), nil)
	out, isErr := r.text(), r.isError
	if isErr || !strings.Contains(out, "echo: ping") {
		t.Fatalf("executive MCP call should round-trip: out=%q isErr=%v", out, isErr)
	}

	// Plan mode PROMPTS (effectiveMode forces ask) — the user's Cancel denies, their Execute runs.
	planning := &Session{mcp: m, planCtl: planCtlResearching(), mode: permissions.ModeAllowAll, out: io.Discard}
	prompted := 0
	planning.approve = func(_ context.Context, req ApprovalRequest) ApprovalDecision {
		prompted++
		if len(req.RememberScopes) != 2 {
			t.Errorf("MCP card must offer tool+server remember scopes, got %+v", req.RememberScopes)
		}
		return Denied("")
	}
	r = planning.invokeMCP(ctx, mcpOriginDirect, "mcp__fake__echo", json.RawMessage(`{"text":"x"}`), nil)
	out, isErr = r.text(), r.isError
	if prompted != 1 || !isErr || !strings.Contains(out, "denied") {
		t.Fatalf("plan mode must prompt and honor Cancel: prompted=%d out=%q isErr=%v", prompted, out, isErr)
	}
	planning.approve = func(context.Context, ApprovalRequest) ApprovalDecision { return Allowed() }
	r = planning.invokeMCP(ctx, mcpOriginDirect, "mcp__fake__echo", json.RawMessage(`{"text":"go"}`), nil)
	if r.isError || !strings.Contains(r.text(), "echo: go") {
		t.Fatalf("plan mode Execute must run the call: out=%q isErr=%v", r.text(), r.isError)
	}
}

// A remembered scope persists and silences the prompt: the first call answers "Don't ask
// again for <server>", the second call (fresh gate, same project) must not prompt at all,
// and the grant is on disk in the server's approvals record.
func TestInvokeMCPRememberServerPersists(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := connectEchoMCP(t, ctx)
	defer m.Close()

	root := t.TempDir()
	s := &Session{
		mcp: m, planCtl: &plan.Controller{}, mode: permissions.ModeAsk, out: io.Discard,
		root: root, mcpConfigs: map[string]mcp.ServerConfig{"fake": echoServerConfig()},
	}
	prompted := 0
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		prompted++
		return ApprovalDecision{Allow: true, RememberScope: "server"}
	}
	if r := s.invokeMCP(ctx, mcpOriginDirect, "mcp__fake__echo", json.RawMessage(`{"text":"a"}`), nil); r.isError {
		t.Fatalf("first call failed: %s", r.text())
	}
	if prompted != 1 {
		t.Fatalf("first call should prompt exactly once, got %d", prompted)
	}
	if !mcp.LoadApprovals(root).CallAllowed("fake", echoServerConfig(), "echo") {
		t.Fatal("server grant should be persisted to the approvals file")
	}
	s.approve = func(context.Context, ApprovalRequest) ApprovalDecision {
		t.Fatal("a remembered server must not prompt again")
		return Denied("")
	}
	if r := s.invokeMCP(ctx, mcpOriginDirect, "mcp__fake__echo", json.RawMessage(`{"text":"b"}`), nil); r.isError {
		t.Fatalf("granted call failed: %s", r.text())
	}
}

// Cache stability — the tools block must be BYTE-IDENTICAL as servers come and go (that is
// the point of progressive disclosure: a mid-session connect can no longer invalidate the
// cached prefix). Only the volatile facts index line changes.
func TestMCPToolDefsCacheStable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := connectEchoMCP(t, ctx)
	defer m.Close()
	s := &Session{mcp: m, planCtl: &plan.Controller{}}

	before, err := json.Marshal(s.toolDefs())
	if err != nil {
		t.Fatal(err)
	}
	m.Add(ctx, map[string]mcp.ServerConfig{"fake2": echoServerConfig()}, mcp.Options{Version: "test"})
	if !m.Has("mcp__fake2__echo") {
		t.Fatalf("second server should connect: %v", m.Errors())
	}
	after, err := json.Marshal(s.toolDefs())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("tools block changed when a server was added — progressive disclosure must keep it byte-stable")
	}
	if line := s.mcpIndexFact(); !strings.Contains(line, "fake2") {
		t.Errorf("the volatile facts line should pick up the new server instead: %q", line)
	}
}

// The full bridge path: a script discovers tools, then loops an MCP call — ONE prompt covers
// the whole run (Execute is run-scoped), intermediates stay script-side, and only the
// distilled print returns.
func TestCodeExecMCPBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m := connectEchoMCP(t, ctx)
	defer m.Close()

	s := codeExecTestSession(t, permissions.ModeAsk)
	s.mcp = m
	var mu sync.Mutex
	mcpPrompts := 0
	s.approve = func(_ context.Context, req ApprovalRequest) ApprovalDecision {
		mu.Lock()
		defer mu.Unlock()
		if req.Label == "MCP tool" {
			mcpPrompts++
		}
		return Allowed() // the mcp_code_exec script gate itself is also approved here
	}

	script := `
found = search_tools("echo")
parts = [mcp("mcp__fake__echo", text=w) for w in ["a", "b", "c"]]
print("found" if "mcp__fake__echo" in found else "missing", "|".join(parts))
`
	tr := s.codeExec(ctx, codeExecInput(t, map[string]any{"script": script}))
	if tr.isError {
		t.Fatalf("mcp_code_exec failed: %s", tr.text())
	}
	out := tr.text()
	if !strings.Contains(out, "found") || !strings.Contains(out, "echo: a|echo: b|echo: c") {
		t.Fatalf("script should discover and loop the MCP tool: %q", out)
	}
	if mcpPrompts != 1 {
		t.Errorf("three calls to one tool must prompt exactly once (run-scoped Execute), got %d", mcpPrompts)
	}
}

// Cancel is run-scoped too: after one deny, later calls to the same tool fail WITHOUT
// prompting again — a loop can't nag its way to a yes.
func TestCodeExecMCPCancelCoversRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m := connectEchoMCP(t, ctx)
	defer m.Close()

	s := codeExecTestSession(t, permissions.ModeAsk)
	s.mcp = m
	var mu sync.Mutex
	mcpPrompts := 0
	s.approve = func(_ context.Context, req ApprovalRequest) ApprovalDecision {
		mu.Lock()
		defer mu.Unlock()
		if req.Label == "MCP tool" {
			mcpPrompts++
			return Denied("")
		}
		return Allowed()
	}

	script := `
outcomes = []
for w in ["a", "b"]:
    try:
        mcp("mcp__fake__echo", text=w)
        outcomes.append("ran")
    except Exception as e:
        outcomes.append("denied" if "denied" in str(e) else str(e))
print("|".join(outcomes))
`
	tr := s.codeExec(ctx, codeExecInput(t, map[string]any{"script": script}))
	if tr.isError {
		t.Fatalf("mcp_code_exec failed: %s", tr.text())
	}
	if out := tr.text(); !strings.Contains(out, "denied|denied") {
		t.Fatalf("both calls should be denied: %q", out)
	}
	if mcpPrompts != 1 {
		t.Errorf("the second call must auto-deny from the run cache, got %d prompts", mcpPrompts)
	}
}

// The run's cumulative remote-time budget refuses further MCP calls once exhausted.
func TestBridgeMCPBudgetExhausted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := connectEchoMCP(t, ctx)
	defer m.Close()
	s := &Session{mcp: m, planCtl: &plan.Controller{}, mode: permissions.ModeAllowAll, out: io.Discard}

	grants := newMCPRunGrants()
	grants.callTime = codeExecMCPBudget + time.Second
	var res bridgedRun
	var mu sync.Mutex
	var locks sync.Map
	var gotErr string
	reply := func(_ int, _ string, errText string) { gotErr = errText }
	s.bridgeMCPCall(ctx, 1, "mcp__fake__echo", json.RawMessage(`{"text":"x"}`), grants, &locks, &mu, &res, reply)
	if !strings.Contains(gotErr, "budget") {
		t.Fatalf("over-budget call should be refused: %q", gotErr)
	}
	if len(res.mcpCalls) != 0 {
		t.Errorf("a refused call must not reach the server (log: %v)", res.mcpCalls)
	}
}

// An oversized MCP reply truncates at the bridge cap with an explicit marker (never silent).
func TestBridgeMCPReplyTruncates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m := connectEchoMCP(t, ctx)
	defer m.Close()
	s := &Session{mcp: m, planCtl: &plan.Controller{}, mode: permissions.ModeAllowAll, out: io.Discard}

	big, err := json.Marshal(map[string]any{"text": strings.Repeat("x", maxBridgeReply)})
	if err != nil {
		t.Fatal(err)
	}
	grants := newMCPRunGrants()
	var res bridgedRun
	var mu sync.Mutex
	var locks sync.Map
	var gotOut string
	reply := func(_ int, out, _ string) { gotOut = out }
	s.bridgeMCPCall(ctx, 1, "mcp__fake__echo", json.RawMessage(big), grants, &locks, &mu, &res, reply)
	if len(gotOut) > maxBridgeReply+128 {
		t.Fatalf("reply must be capped near %d bytes, got %d", maxBridgeReply, len(gotOut))
	}
	if !strings.Contains(gotOut, "truncated") {
		t.Fatal("truncation must be marked, not silent")
	}
}
