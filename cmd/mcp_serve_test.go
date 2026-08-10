package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/memcode-ai/memcode/internal/agent/introspect"
)

// The MCP memory server must advertise its tools and answer the offline
// code-location tool — this is the surface a Claude Code / Cursor user connects
// to, so the tool set and a working call are the contract.
func TestMCPServeMemoryTools(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.go"),
		[]byte("package a\n\nfunc VerifyToken(tok string) bool { return tok != \"\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// memcode_locate uses only root; the memory tools that need a store are not
	// exercised here, so a minimal engine is enough to register the surface.
	eng := introspect.New(introspect.Deps{Root: dir})
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "memcode", Version: "test"}, nil)
	registerMemoryTools(srv, eng, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()

	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := map[string]bool{
		"memcode_recall": false, "memcode_overview": false, "memcode_claims": false,
		"memcode_session": false, "memcode_locate": false,
	}
	for _, tl := range tools.Tools {
		if _, ok := want[tl.Name]; ok {
			want[tl.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q not advertised", name)
		}
	}

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "memcode_locate",
		Arguments: map[string]any{"query": "verify token"},
	})
	if err != nil {
		t.Fatalf("call memcode_locate: %v", err)
	}
	if res.IsError {
		t.Fatalf("memcode_locate returned an error result")
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			text += tc.Text
		}
	}
	if !strings.Contains(text, "auth.go") {
		t.Errorf("memcode_locate didn't find auth.go; got: %s", text)
	}
}
