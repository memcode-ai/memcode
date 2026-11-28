package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain re-routes into a minimal in-binary LSP server when LSP_TEST_SERVER=1, so the
// client can be exercised over a REAL stdio transport with no gopls/pyright installed
// (the same pattern the MCP e2e test uses).
func TestMain(m *testing.M) {
	if os.Getenv("LSP_TEST_SERVER") == "1" {
		runStubServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runStubServer speaks just enough LSP to prove the client: initialize, didOpen (→ push
// one diagnostic), definition, references, hover, shutdown/exit.
func runStubServer() {
	r := bufio.NewReader(os.Stdin)
	reply := func(id int, result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	push := func(method string, params any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	loc := Location{URI: "file:///w/x.go", Range: Range{Start: Position{Line: 2, Character: 5}, End: Position{Line: 2, Character: 8}}}
	for {
		msg, err := readMessage(r)
		if err != nil {
			return
		}
		var req struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(msg, &req)
		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]any{"capabilities": map[string]any{}})
		case "textDocument/didOpen":
			// Echo diagnostics back for the URI that was actually opened (so the manager,
			// which opens a real temp file, sees them for its own uri).
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(req.Params, &p)
			push("textDocument/publishDiagnostics", map[string]any{
				"uri": p.TextDocument.URI,
				"diagnostics": []Diagnostic{
					{Range: loc.Range, Severity: 1, Message: "undefined: Foo", Source: "stub"},
				},
			})
		case "textDocument/definition":
			reply(req.ID, loc)
		case "textDocument/references":
			reply(req.ID, []Location{loc, {URI: "file:///w/y.go", Range: loc.Range}})
		case "textDocument/hover":
			reply(req.ID, map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func Foo() int"}})
		case "test/hang":
			// never replies: the client's timeout must clean up after itself
		case "test/delayed":
			time.Sleep(300 * time.Millisecond)
			reply(req.ID, "late")
		case "test/die":
			os.Exit(3)
		case "shutdown":
			reply(req.ID, nil)
		case "exit":
			return
		}
	}
}

func newStubClientWithEnv(t *testing.T) *Client {
	t.Helper()
	c, err := newClient(context.Background(), t.TempDir(), []string{"LSP_TEST_SERVER=1"}, os.Args[0], "-test.run=TestNoSuchTest")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientRoundTrip(t *testing.T) {
	c := newStubClientWithEnv(t)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Initialize(ctx, "/w"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := c.DidOpen("file:///w/x.go", "go", "package w\n\nvar _ = Foo\n"); err != nil {
		t.Fatalf("didOpen: %v", err)
	}
	// Diagnostics arrive as an async push after didOpen.
	diags := c.WaitDiagnostics(ctx, "file:///w/x.go", 3*time.Second)
	if len(diags) != 1 || diags[0].Message != "undefined: Foo" || diags[0].SeverityLabel() != "error" {
		t.Fatalf("diagnostics: %+v", diags)
	}
	// Definition (single Location).
	defs, err := c.Definition(ctx, "file:///w/x.go", Position{Line: 2, Character: 6})
	if err != nil || len(defs) != 1 {
		t.Fatalf("definition: %v %+v", err, defs)
	}
	// References (array).
	refs, err := c.References(ctx, "file:///w/x.go", Position{Line: 2, Character: 6})
	if err != nil || len(refs) != 2 {
		t.Fatalf("references: %v %+v", err, refs)
	}
	// Hover (MarkupContent → text).
	h, err := c.Hover(ctx, "file:///w/x.go", Position{Line: 2, Character: 6})
	if err != nil || h != "func Foo() int" {
		t.Fatalf("hover: %v %q", err, h)
	}
}

func TestPathURIRoundTrip(t *testing.T) {
	uri := PathToURI("/w/x.go")
	if uri != "file:///w/x.go" {
		t.Errorf("PathToURI = %q", uri)
	}
	if p := URIToPath(uri); p != "/w/x.go" {
		t.Errorf("URIToPath = %q", p)
	}
}

// A call that times out must not leak its pending entry.
func TestCallTimeoutForgetsPending(t *testing.T) {
	c := newStubClientWithEnv(t)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.call(ctx, "test/hang", nil); err == nil {
		t.Fatal("hang call returned without error")
	}
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending entries after timeout = %d, want 0", n)
	}
}

// A writer stuck on stdin (simulated by holding the write lock) must not stop the read
// loop from delivering responses to other callers.
func TestBlockedWriterDoesNotStallReads(t *testing.T) {
	c := newStubClientWithEnv(t)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan error, 1)
	go func() {
		_, err := c.call(ctx, "test/delayed", nil)
		got <- err
	}()
	time.Sleep(100 * time.Millisecond) // the request is written; its reply is due in ~200ms
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("delayed call: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("response not delivered while a writer held the write lock")
	}
}

// A server that dies on its own is reaped (no zombie), and pending calls fail fast.
func TestExitedServerIsReaped(t *testing.T) {
	c := newStubClientWithEnv(t)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.call(ctx, "test/die", nil); err == nil {
		t.Fatal("call to a dying server succeeded")
	}
	select {
	case <-c.exited:
	case <-time.After(3 * time.Second):
		t.Fatal("exited server was not reaped")
	}
	if c.cmd.ProcessState == nil {
		t.Fatal("no ProcessState: Wait was never called")
	}
}

// Close on a live server returns promptly and reaps it.
func TestCloseReaps(t *testing.T) {
	c := newStubClientWithEnv(t)
	start := time.Now()
	c.Close()
	if time.Since(start) > 6*time.Second {
		t.Fatalf("Close took %v", time.Since(start))
	}
	select {
	case <-c.exited:
	default:
		t.Fatal("Close returned before the server was reaped")
	}
}
