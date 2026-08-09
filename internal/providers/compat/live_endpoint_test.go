package compat

// live_endpoint_test.go — the Phase C acceptance E2E: a FULL turn against a
// real local OpenAI-compat endpoint (Ollama by default) through the exact
// transport the CLI ships, with every HTTP request instrumented to prove ZERO
// traffic reaches any memcode host. Env-gated so ordinary runs skip:
//
//	MEMCODE_E2E_ENDPOINT=1 \
//	go test ./internal/provider/compat -run LiveEndpoint -v
//
// Optional overrides:
//
//	MEMCODE_E2E_ENDPOINT_URL    compat base (default http://localhost:11434/v1)
//	MEMCODE_E2E_ENDPOINT_KEY    bearer key (default none — local endpoints)
//	MEMCODE_E2E_ENDPOINT_MODEL  model id (default: autodetected from GET
//	                            {base}/models, preferring tool-capable families)
//
// The same suite doubles as the OpenAI/Groq acceptance run — point it at a
// cloud endpoint with a key:
//
//	MEMCODE_E2E_ENDPOINT=1 \
//	MEMCODE_E2E_ENDPOINT_URL=https://api.openai.com/v1 \
//	MEMCODE_E2E_ENDPOINT_KEY=sk-… \
//	MEMCODE_E2E_ENDPOINT_MODEL=gpt-… \
//	go test ./internal/provider/compat -run LiveEndpoint -v
//
// (Groq: https://api.groq.com/openai/v1 — the path-prefix precedent.)
//
// The wire-contract matrix itself (HARD/preferred/capability tiers) lives in
// the Phase A0 conformance suite — run it against the same base for the full
// picture:
//
//	cd api && CONFORMANCE_BASE_URL=http://localhost:11434/v1 \
//	go test ./internal/compat/conformance -run Conformance -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/wire"
)

// hostRecorder instruments the transport: every request's host is recorded,
// and any memcode host fails the test — the no-debit / no-phone-home proof.
type hostRecorder struct {
	t     *testing.T
	next  http.RoundTripper
	mu    sync.Mutex
	hosts map[string]int
}

func (h *hostRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	if h.hosts == nil {
		h.hosts = map[string]int{}
	}
	h.hosts[r.URL.Host]++
	h.mu.Unlock()
	if strings.Contains(strings.ToLower(r.URL.Host), "memcode") {
		h.t.Errorf("endpoint-mode turn reached a memcode host: %s %s", r.Method, r.URL)
	}
	return h.next.RoundTrip(r)
}

func (h *hostRecorder) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.hosts))
	for host, n := range h.hosts {
		out = append(out, fmt.Sprintf("%s×%d", host, n))
	}
	sort.Strings(out)
	return out
}

// pickEndpointModel autodetects a chat/tool-capable model from GET
// {base}/models, preferring families with reliable tool calling.
func pickEndpointModel(ctx context.Context, base, key string, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return "", err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return "", err
	}
	if len(list.Data) == 0 {
		return "", fmt.Errorf("endpoint listed no models")
	}
	for _, pref := range []string{"qwen3", "qwen", "llama3.", "mistral", "glm"} {
		for _, m := range list.Data {
			if strings.Contains(strings.ToLower(m.ID), pref) {
				return m.ID, nil
			}
		}
	}
	return list.Data[0].ID, nil
}

// TestLiveEndpointFullTurn drives the agent's real turn shape — two system
// messages (the doctrine convention), streamed leg with tool defs, then the
// tool_result leg — against the local endpoint, asserting text, the tool
// round-trip where the model supports it, and zero memcode traffic.
func TestLiveEndpointFullTurn(t *testing.T) {
	if os.Getenv("MEMCODE_E2E_ENDPOINT") == "" {
		t.Skip("live endpoint E2E skipped: set MEMCODE_E2E_ENDPOINT=1 (see file header)")
	}
	base := os.Getenv("MEMCODE_E2E_ENDPOINT_URL")
	if base == "" {
		base = "http://localhost:11434/v1"
	}
	key := os.Getenv("MEMCODE_E2E_ENDPOINT_KEY")

	rec := &hostRecorder{t: t, next: http.DefaultTransport}
	client := &http.Client{Timeout: 5 * time.Minute, Transport: rec}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	model := os.Getenv("MEMCODE_E2E_ENDPOINT_MODEL")
	if model == "" {
		var err error
		if model, err = pickEndpointModel(ctx, base, key, client); err != nil {
			t.Fatalf("model autodetect against %s failed: %v (set MEMCODE_E2E_ENDPOINT_MODEL)", base, err)
		}
		t.Logf("autodetected model %q", model)
	}

	// The endpoint-mode transport exactly as dialEndpoint builds it: pure
	// standard wire, keyless unless configured, the session model as default.
	tr := New(Config{BaseURL: base, Token: key, Memcode: false, Model: model, HTTPClient: client})

	readFileTool := wire.ToolDef{
		Name:        "read_file",
		Description: "Read a file from the repository and return its contents.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "repository-relative file path"},
			},
			"required": []any{"path"},
		},
	}
	history := []wire.Message{
		{Role: "user", Blocks: []wire.Block{wire.TextBlock(
			"Use the read_file tool to read VERSION, then tell me what it contains. You must call the tool first.")}},
	}
	turn := func() (wire.Response, int, error) {
		var deltas int
		resp, err := tr.Stream(ctx, wire.Request{
			System:         "You are a terse coding assistant working in a repository.",
			SystemVolatile: "[today: 2026-08-08] Answer in one short sentence.",
			Session:        fmt.Sprintf("endpoint-e2e-%d", time.Now().UnixNano()),
			Tools:          []wire.ToolDef{readFileTool},
			Messages:       history,
		}, wire.StreamHandler{Text: func(string) { deltas++ }})
		return resp, deltas, err
	}

	// Leg 1: the model should call the tool (streamed deltas either way).
	resp, deltas, err := turn()
	if err != nil {
		t.Fatalf("leg 1 against %s (%s) failed: %v", base, model, err)
	}
	if resp.Model == "" {
		t.Log("endpoint reported no model id on the stream (tolerated)")
	}
	if resp.Backend != "endpoint" {
		t.Errorf("response backend = %q, want endpoint", resp.Backend)
	}

	uses := resp.ToolUses()
	switch {
	case len(uses) > 0:
		// Tool round-trip: answer the call, expect a final text leg.
		u := uses[0]
		if u.Name != "read_file" {
			t.Errorf("model called %q, offered only read_file", u.Name)
		}
		t.Logf("leg 1: tool call %s(%s), stop=%s", u.Name, string(u.Input), resp.StopReason)
		history = append(history,
			wire.Message{Role: "assistant", Blocks: resp.Blocks},
			wire.Message{Role: "user", Blocks: []wire.Block{{
				Type: "tool_result", ToolUseID: u.ID, Content: "0.4.0\n",
			}}},
		)
		final, finalDeltas, err := turn()
		if err != nil {
			t.Fatalf("tool_result leg failed: %v", err)
		}
		if strings.TrimSpace(final.Text()) == "" {
			t.Error("final leg returned no assistant text")
		}
		if finalDeltas == 0 {
			t.Error("final leg streamed no text deltas")
		}
		if !strings.Contains(final.Text(), "0.4.0") {
			t.Logf("final text does not echo the file content (tolerated — model quality): %q", final.Text())
		}
		t.Logf("leg 2: %q (deltas=%d, in=%d out=%d)", strings.TrimSpace(final.Text()), finalDeltas, final.InputTokens, final.OutputTokens)
	case strings.TrimSpace(resp.Text()) != "":
		// Honest degradation: the model answered in prose without touching the
		// tool — text works, tool calling doesn't. Report, don't fake it.
		t.Logf("model %q did not call the tool (answered %q) — text path verified, tool round-trip UNSUPPORTED on this model", model, strings.TrimSpace(resp.Text()))
		if deltas == 0 {
			t.Error("no streamed text deltas arrived")
		}
	default:
		t.Fatalf("leg 1 returned neither text nor a tool call: %+v", resp)
	}

	// Usage may be absent on some endpoints (the CLI estimates locally);
	// Ollama's compat reports it — log the truth either way.
	t.Logf("usage: in=%d cacheRead=%d out=%d", resp.InputTokens, resp.CacheReadTokens, resp.OutputTokens)

	// The no-memcode-traffic proof: every request in the run went through the
	// recorder; the RoundTrip assertion already failed on any memcode host.
	t.Logf("hosts contacted: %s", strings.Join(rec.seen(), ", "))
	if len(rec.seen()) == 0 {
		t.Error("instrumentation recorded no requests — the proof did not run")
	}
}
