package conformance

// selfhost_test.go — the conformance suite pointed at OUR OWN gateway, in
// process, in normal CI: Phase A0 (the contract) and Phase A (the endpoint)
// verify each other. The gateway is the real server.New handler (auth door,
// translation, metering, sanitization); only the model is scripted — and the
// script is HONEST: every answer is derivable only from what actually arrived
// on the internal wire (the codeword must arrive in the volatile system half,
// the PDF bytes must arrive intact in a document block, the forced tool name
// must arrive in ToolChoice). So a translation regression fails the matching
// conformance check, not a mock.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/gateway/internal/server"
	"github.com/memcode-ai/memcode/internal/wire"
)

var codewordRe = regexp.MustCompile(`codeword is ([A-Z0-9-]+)`)

// scriptedProvider answers each conformance probe from evidence on the
// internal request — never from knowledge of the suite's expectations alone.
type scriptedProvider struct{}

func (scriptedProvider) respond(r wire.Request) wire.Response {
	base := wire.Response{
		Model: r.Model, Backend: "anthropic",
		InputTokens: 42, OutputTokens: 7, CacheReadTokens: 5,
	}
	text := func(s string) wire.Response {
		out := base
		out.StopReason = "end_turn"
		out.Blocks = []wire.Block{wire.TextBlock(s)}
		return out
	}
	// Forced tool_choice: call exactly the tool the wire forced.
	if r.ToolChoice != "" {
		out := base
		out.StopReason = "tool_use"
		out.Blocks = []wire.Block{{
			Type: "tool_use", ID: "call_1", Name: r.ToolChoice,
			Input: json.RawMessage(`{"verdict":"yes","reason":"probe"}`),
		}}
		return out
	}
	// Codeword: only answerable if the SECOND system message landed in the
	// volatile half (the two-system convention).
	if m := codewordRe.FindStringSubmatch(r.SystemVolatile); m != nil {
		return text(m[1])
	}
	for _, msg := range r.Messages {
		for _, b := range msg.Blocks {
			switch b.Type {
			case "image":
				// Only answerable if the image part arrived as a vision block.
				if b.Source != nil && b.Source.MediaType == "image/png" {
					return text("red")
				}
				return text("no image data")
			case "document":
				// Only answerable if the PDF bytes arrived intact.
				if b.Source != nil {
					raw, err := base64.StdEncoding.DecodeString(b.Source.Data)
					if err == nil && bytes.Contains(raw, []byte("EMBER-NINE")) {
						return text("EMBER-NINE")
					}
				}
				return text("unreadable document")
			}
		}
	}
	if len(r.Messages) > 0 {
		last := r.Messages[len(r.Messages)-1]
		for _, b := range last.Blocks {
			if strings.Contains(b.Text, "Count from 1 to 5") {
				return text("1 2 3 4 5")
			}
		}
	}
	return text("pong")
}

func (p scriptedProvider) Complete(_ context.Context, r wire.Request) (wire.Response, error) {
	return p.respond(r), nil
}

func (p scriptedProvider) Stream(_ context.Context, r wire.Request, h wire.StreamHandler) (wire.Response, error) {
	out := p.respond(r)
	if h.Text != nil {
		for _, b := range out.Blocks {
			if b.Type == "text" && b.Text != "" {
				// two deltas per block so the suite sees real chunking
				mid := len(b.Text) / 2
				h.Text(b.Text[:mid])
				h.Text(b.Text[mid:])
			}
		}
	}
	if h.Usage != nil {
		h.Usage(out.InputTokens, out.OutputTokens)
	}
	return out, nil
}

// selfGateway boots the real gateway handler over the scripted provider, with
// a stub web app accepting any bearer as a funded org.
func selfGateway(t *testing.T) *httptest.Server {
	t.Helper()
	// The strict label gate reads the deployment's credential env — pin it so
	// the suite is deterministic regardless of the developer's ambient shell.
	t.Setenv("MEMCODE_PROVIDER", "hybrid")
	t.Setenv("OPENAI_API_KEY", "sk-oa")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	t.Setenv("XAI_API_KEY", "sk-grok")
	t.Setenv("GEMINI_API_KEY", "sk-gem")
	t.Setenv("MEMCODE_FIREWORKS_URL", "https://fw.example/v1")
	gw := httptest.NewServer(server.New(server.Config{
		SelfHostToken: "memcode_selftest_key",
		Provider:      scriptedProvider{},
		BackendName:   "test",
	}))
	t.Cleanup(gw.Close)
	return gw
}

// TestSelfConformance holds the gateway's compat surface to the full Phase A0
// contract in normal CI — hard tier must pass, and the probed tiers must all
// report supported (the gateway serves usage, models, vision, files, effort,
// and cached tokens).
func TestSelfConformance(t *testing.T) {
	gw := selfGateway(t)
	// A concrete servable label — there is no server-side Automatic anymore
	// (the CLI's selection policy decides; the gateway serves what's asked).
	c := newClient(gw.URL+"/v1", "memcode_selftest_key", "sonnet")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	m := runSuite(ctx, t, c)
	t.Log("\n" + m.String())

	// The gateway is our own reference endpoint: EVERY row must be green, the
	// optional tiers included.
	for _, r := range m.rows {
		if !r.ok {
			t.Errorf("gateway self-conformance: %s / %s not supported (%s)", r.tier, r.name, r.note)
		}
	}
}
