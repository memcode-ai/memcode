// Package lsp is memcode's resident Language Server Protocol client — the "give the
// agent eyes" layer. It speaks JSON-RPC 2.0 over a language server's stdio (gopls,
// typescript-language-server, pyright), holding the server RESIDENT for the session so
// diagnostics are incremental (milliseconds, not a full re-typecheck) and semantic
// queries (definition / references / hover) are available. This is the capability no
// amount of grep + build gives, and the only static type-error source for TS/Python.
//
// Model (following Claude Code): DETECT AND CONNECT. A server is used only when its
// binary is on PATH; nothing is bundled or auto-installed. The valuable operations for
// an agent are diagnostics, definition, and references — completion is deliberately not
// implemented.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is a JSON-RPC 2.0 client over one language server's stdio. It is created by the
// manager per language and reused across a session. Safe for concurrent Requests; the
// write side is mutex-guarded and responses are demuxed by id.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResponse

	diagMu sync.Mutex
	diags  map[string][]Diagnostic // uri → latest published diagnostics

	closeOnce sync.Once
	closed    chan struct{}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNotify struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     int             `json:"id"`
	Method string          `json:"method"` // set on server→client notifications (id absent)
	Result json.RawMessage `json:"result"`
	Params json.RawMessage `json:"params"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message) }

// newClient starts the server command in dir (the workspace root) and begins reading its
// output. It does NOT initialize — the manager drives the initialize handshake so it can
// pass the workspace root. extraEnv (nil in production) is appended to the environment;
// tests use it to re-route into the in-binary stub server. Errors if the binary can't start.
func newClient(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (*Client, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard // servers are chatty on stderr; the protocol is on stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		pending: map[int]chan rpcResponse{},
		diags:   map[string][]Diagnostic{},
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// readLoop reads Content-Length-framed messages and dispatches them: a message with an
// id resolves a pending Request; a message with a method (no id) is a server
// notification (we care about textDocument/publishDiagnostics).
func (c *Client) readLoop() {
	defer close(c.closed)
	for {
		msg, err := readMessage(c.stdout)
		if err != nil {
			return // stream closed (server exited) — pending requests unblock via ctx/timeout
		}
		var resp rpcResponse
		if json.Unmarshal(msg, &resp) != nil {
			continue
		}
		if resp.Method == "textDocument/publishDiagnostics" {
			c.handleDiagnostics(resp.Params)
			continue
		}
		if resp.Method != "" {
			continue // an unrelated server notification/request — ignore (we don't register capabilities that need replies)
		}
		c.mu.Lock()
		ch := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

func (c *Client) handleDiagnostics(params json.RawMessage) {
	var p struct {
		URI         string       `json:"uri"`
		Diagnostics []Diagnostic `json:"diagnostics"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	c.diagMu.Lock()
	c.diags[p.URI] = p.Diagnostics
	c.diagMu.Unlock()
}

// call sends a request and waits for its response (or ctx cancel). The server processes
// requests in order; we demux by id so concurrent callers are safe.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, fmt.Errorf("lsp server exited")
	}
}

// notify sends a notification (no response expected).
func (c *Client) notify(method string, params any) error {
	return c.write(rpcNotify{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) write(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

// Close shuts the server down (best-effort shutdown/exit, then kill).
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = c.call(ctx, "shutdown", nil)
		_ = c.notify("exit", nil)
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
}

// readMessage reads one Content-Length-framed JSON-RPC message from r.
func readMessage(r *bufio.Reader) ([]byte, error) {
	var length int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			length, err = strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length: %w", err)
			}
		}
	}
	if length <= 0 {
		return nil, fmt.Errorf("missing/zero Content-Length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
