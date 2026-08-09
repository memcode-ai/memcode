package mcp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This is the load-bearing proof that an LLM can actually USE an MCP server: a real subprocess
// over the real stdio CommandTransport, dialed by the production Manager, with a tool discovered
// and called end-to-end. There is NO production test seam — when MCP_TEST_SERVER=1 the test binary
// re-execs as a tiny MCP server (the os/exec TestHelperProcess idiom), so Connect spawns it exactly
// as it would a real `npx ...` server.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_TEST_SERVER") == "1" {
		runEchoServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runEchoServer is the in-binary MCP server: one "echo" tool that returns its `text` argument,
// and an "echo.upper" tool (a deliberately dot-named tool) to prove name-sanitization round-trips.
func runEchoServer() {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "echo-server", Version: "v0.0.1"}, nil)
	schema := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
	echo := func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args struct{ Text string }
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo: " + args.Text}}}, nil
	}
	srv.AddTool(&mcpsdk.Tool{Name: "echo", Description: "echo the text back", InputSchema: schema}, echo)
	srv.AddTool(&mcpsdk.Tool{Name: "echo.upper", Description: "dot-named tool", InputSchema: schema},
		func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			r, err := echo(ctx, req)
			return r, err
		})
	// A resource (external context) and a prompt template — the optional MCP capabilities.
	srv.AddResource(&mcpsdk.Resource{URI: "mem://doc", Name: "doc", Description: "a doc", MIMEType: "text/plain"},
		func(_ context.Context, _ *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: "mem://doc", MIMEType: "text/plain", Text: "hello from the resource"}}}, nil
		})
	srv.AddPrompt(&mcpsdk.Prompt{Name: "greet", Description: "greet someone",
		Arguments: []*mcpsdk.PromptArgument{{Name: "who", Required: true}}},
		func(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
			return &mcpsdk.GetPromptResult{Messages: []*mcpsdk.PromptMessage{
				{Role: "user", Content: &mcpsdk.TextContent{Text: "Hello, " + req.Params.Arguments["who"]}},
			}}, nil
		})
	_ = srv.Run(context.Background(), &mcpsdk.StdioTransport{})
}

// helperServer returns the stdio ServerConfig that dials this test binary as an MCP server.
func helperServer() ServerConfig {
	return ServerConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestNoSuchTest"}, // run no tests; TestMain re-routes on the env var
		Env:     map[string]string{"MCP_TEST_SERVER": "1"},
	}
}

func TestManagerStdioRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	m := Connect(ctx, map[string]ServerConfig{"fake": helperServer()}, Options{Version: "test"})
	defer m.Close()
	if errs := m.Errors(); len(errs) != 0 {
		t.Fatalf("connect should be clean, got %v", errs)
	}

	// Tools are discovered and namespaced for the model.
	tools := m.Tools()
	byName := map[string]Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	echo, ok := byName["mcp__fake__echo"]
	if !ok {
		t.Fatalf("echo tool not discovered; got %v", toolNames(tools))
	}
	if echo.Server != "fake" || echo.Raw != "echo" || echo.InputSchema["type"] != "object" {
		t.Fatalf("tool not flattened correctly: %+v", echo)
	}
	// The dot-named raw tool must surface under a SANITIZED model-facing name (no '.'), while its
	// Raw stays the real server name so the call still resolves.
	var dot Tool
	for _, tl := range tools {
		if tl.Raw == "echo.upper" {
			dot = tl
		}
	}
	if dot.Name == "" || strings.Contains(dot.Name, ".") {
		t.Fatalf("dot-named tool must be sanitized for the model: %+v", dot)
	}

	// The model-facing name actually CALLS the tool (proves byName→raw round-trip).
	out, isErr, err := m.Call(ctx, "mcp__fake__echo", map[string]any{"text": "ping"})
	if err != nil || isErr {
		t.Fatalf("call failed: out=%q isErr=%v err=%v", out, isErr, err)
	}
	if !strings.Contains(out, "echo: ping") {
		t.Fatalf("unexpected echo result: %q", out)
	}
	if out2, _, err := m.Call(ctx, dot.Name, map[string]any{"text": "z"}); err != nil || !strings.Contains(out2, "echo: z") {
		t.Fatalf("sanitized-name call must reach the dot-named tool: out=%q err=%v", out2, err)
	}

	// Has recognizes the model-facing name; an unknown tool errors rather than panicking.
	if !m.Has("mcp__fake__echo") {
		t.Error("Has should recognize a discovered tool")
	}
	if _, _, err := m.Call(ctx, "mcp__fake__nope", nil); err == nil {
		t.Error("calling an unknown tool should error")
	}
}

// Resources and prompts (the optional MCP capabilities) are discovered on connect and
// round-trip through the manager: a resource reads its contents, a prompt renders with args.
func TestManagerResourcesAndPrompts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m := Connect(ctx, map[string]ServerConfig{"fake": helperServer()}, Options{Version: "test"})
	defer m.Close()

	rs := m.Resources()
	if len(rs) != 1 || rs[0].URI != "mem://doc" || rs[0].Server != "fake" {
		t.Fatalf("resource not discovered: %+v", rs)
	}
	body, err := m.ReadResource(ctx, "mem://doc")
	if err != nil || !strings.Contains(body, "hello from the resource") {
		t.Fatalf("ReadResource: body=%q err=%v", body, err)
	}
	if _, err := m.ReadResource(ctx, "mem://nope"); err == nil {
		t.Error("reading an unknown resource should error")
	}

	ps := m.Prompts()
	if len(ps) != 1 || ps[0].Name != "mcp__fake__greet" || len(ps[0].Arguments) != 1 {
		t.Fatalf("prompt not discovered: %+v", ps)
	}
	out, err := m.GetPrompt(ctx, "mcp__fake__greet", map[string]string{"who": "Tim"})
	if err != nil || !strings.Contains(out, "Hello, Tim") {
		t.Fatalf("GetPrompt: out=%q err=%v", out, err)
	}
}

func TestManagerConnectFailureIsNonFatal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A command that can't be an MCP server: it must not sink the Manager — the error is recorded,
	// the Manager is usable (just empty).
	m := Connect(ctx, map[string]ServerConfig{
		"broken": {Command: os.Args[0], Args: []string{"-test.run=TestNoSuchTest"}}, // no MCP_TEST_SERVER → not a server
	}, Options{Version: "test"})
	defer m.Close()
	if len(m.Tools()) != 0 {
		t.Errorf("a non-server command should yield no tools, got %v", toolNames(m.Tools()))
	}
	if len(m.Errors()) == 0 {
		t.Error("a failed connection should be recorded in Errors(), not panic")
	}
}

func toolNames(ts []Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}
