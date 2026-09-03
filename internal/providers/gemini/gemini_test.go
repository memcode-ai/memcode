package gemini

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/providers/provcore"
	"github.com/memcode-ai/memcode/internal/wire"
	"google.golang.org/genai"
)

// A RESOURCE_EXHAUSTED genai error (Gemini's 429) must be recognized by the
// shared retry kernel — via the extractor this package registers at init —
// so rate limiting retries instead of failing the turn (or, worse, being
// misclassified as context overflow).
func TestGeminiResourceExhaustedRetriesAs429(t *testing.T) {
	err := fmt.Errorf("gemini stream: %w",
		&genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "quota exceeded"})
	code, _, ok := provcore.APIErrorInfo(err)
	if !ok || code != 429 {
		t.Fatalf("APIErrorInfo = (%d, %v), want (429, true)", code, ok)
	}
	if !provcore.IsRetryable(code) {
		t.Fatal("429 must be retryable")
	}
	if isGeminiOverflow(err) {
		t.Fatal("RESOURCE_EXHAUSTED must not classify as context overflow")
	}
}

// TestGeminiModelReturnsFlash locks that Model() returns the balanced-tier id.
func TestGeminiModelReturnsFlash(t *testing.T) {
	g := NewGemini("key")
	if g.Model() != catalog.ModelGeminiFlash {
		t.Errorf("Model() = %q, want %q", g.Model(), catalog.ModelGeminiFlash)
	}
}

// TestGeminiMapEffort locks the Effort → ThinkingConfig.ThinkingBudget mapping.
func TestGeminiMapEffort(t *testing.T) {
	g := NewGemini("key")
	cases := []struct {
		eff      wire.Effort
		wantZero bool // true = budget is 0 (thinking off)
		wantNil  bool // true = config is nil (not applicable — always set here)
	}{
		{wire.EffortHigh, false, false},
		{wire.EffortMedium, false, false},
		{wire.EffortLow, false, false},
		{wire.EffortOff, true, false}, // 0 = thinking disabled
	}
	for _, c := range cases {
		tc := g.mapEffort(c.eff)
		if tc == nil {
			t.Errorf("mapEffort(%v) = nil, want non-nil", c.eff)
			continue
		}
		if c.wantZero {
			if tc.ThinkingBudget == nil || *tc.ThinkingBudget != 0 {
				t.Errorf("mapEffort(%v): budget = %v, want 0", c.eff, tc.ThinkingBudget)
			}
		} else {
			if tc.ThinkingBudget == nil || *tc.ThinkingBudget <= 0 {
				t.Errorf("mapEffort(%v): budget = %v, want > 0", c.eff, tc.ThinkingBudget)
			}
		}
	}
}

// TestGeminiBuildContents locks the Request → []*genai.Content mapping: text →
// Text part, tool_use → FunctionCall, tool_result → FunctionResponse, image →
// InlineData, thinking → dropped.
func TestGeminiBuildContents(t *testing.T) {
	g := NewGemini("key")
	r := wire.Request{
		Messages: []wire.Message{
			{Role: "user", Blocks: []wire.Block{
				{Type: "text", Text: "hello"},
			}},
			{Role: "assistant", Blocks: []wire.Block{
				{Type: "text", Text: "reply"},
				{Type: "tool_use", ID: "tu_1", Name: "read_file", Input: []byte(`{"path":"x.go"}`)},
			}},
			{Role: "user", Blocks: []wire.Block{
				{Type: "tool_result", ToolUseID: "tu_1", Name: "read_file", Content: "file contents"},
			}},
		},
	}
	contents := g.buildContents(r)
	if len(contents) != 3 {
		t.Fatalf("buildContents: got %d contents, want 3", len(contents))
	}
	// First: user text → Role "user", one Text part.
	if contents[0].Role != "user" {
		t.Errorf("content[0].Role = %q, want \"user\"", contents[0].Role)
	}
	if len(contents[0].Parts) != 1 || contents[0].Parts[0].Text != "hello" {
		t.Errorf("content[0] text = %+v, want one Text part \"hello\"", contents[0].Parts)
	}
	// Second: assistant → Role "model", text + FunctionCall.
	if contents[1].Role != "model" {
		t.Errorf("content[1].Role = %q, want \"model\"", contents[1].Role)
	}
	if len(contents[1].Parts) != 2 {
		t.Fatalf("content[1] parts = %d, want 2 (text + function_call)", len(contents[1].Parts))
	}
	if contents[1].Parts[1].FunctionCall == nil || contents[1].Parts[1].FunctionCall.Name != "read_file" {
		t.Errorf("content[1] function_call = %+v, want read_file", contents[1].Parts[1].FunctionCall)
	}
	// Third: tool_result → FunctionResponse.
	if len(contents[2].Parts) != 1 || contents[2].Parts[0].FunctionResponse == nil {
		t.Fatalf("content[2] should be a FunctionResponse, got %+v", contents[2].Parts)
	}
	if contents[2].Parts[0].FunctionResponse.Name != "read_file" {
		t.Errorf("content[2] function_response.Name = %q, want \"read_file\"", contents[2].Parts[0].FunctionResponse.Name)
	}
}

// A real tool_result block from the CLI carries only ToolUseID (no Name). Gemini's
// FunctionResponse REQUIRES a name matching the call, so the name must be resolved
// from the preceding tool_use — without it Vertex 400s on every turn-2 tool response.
func TestGeminiToolResultResolvesName(t *testing.T) {
	g := NewGemini("key")
	r := wire.Request{
		Messages: []wire.Message{
			{Role: "assistant", Blocks: []wire.Block{
				{Type: "tool_use", ID: "tu_9", Name: "ripgrep", Input: []byte(`{"q":"x"}`)},
			}},
			{Role: "user", Blocks: []wire.Block{
				// No Name here — exactly what the CLI sends.
				{Type: "tool_result", ToolUseID: "tu_9", Content: "match"},
			}},
		},
	}
	contents := g.buildContents(r)
	fr := contents[len(contents)-1].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "ripgrep" {
		t.Fatalf("FunctionResponse.Name = %v, want \"ripgrep\" (resolved from the tool_use id)", fr)
	}
}

// TestGeminiBuildContentsDropsThinking locks that thinking blocks are dropped
// (Gemini has no cross-turn reasoning round-trip, same as the oaClient treatment).
func TestGeminiBuildContentsDropsThinking(t *testing.T) {
	g := NewGemini("key")
	r := wire.Request{
		Messages: []wire.Message{
			{Role: "assistant", Blocks: []wire.Block{
				{Type: "thinking", Thinking: "reasoning here", Signature: "sig"},
				{Type: "text", Text: "reply"},
			}},
		},
	}
	contents := g.buildContents(r)
	if len(contents) != 1 {
		t.Fatalf("buildContents: got %d contents, want 1", len(contents))
	}
	// Only the text part should survive — thinking is dropped.
	if len(contents[0].Parts) != 1 || contents[0].Parts[0].Text != "reply" {
		t.Errorf("thinking not dropped: parts = %+v", contents[0].Parts)
	}
}

// TestGeminiBuildTools locks the ToolDef → FunctionDeclaration mapping.
func TestGeminiBuildTools(t *testing.T) {
	g := NewGemini("key")
	r := wire.Request{
		Tools: []wire.ToolDef{
			{Name: "read_file", Description: "read a file", InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []any{"path"},
			}},
		},
	}
	tools := g.buildTools(r)
	if len(tools) != 1 {
		t.Fatalf("buildTools: got %d tools, want 1", len(tools))
	}
	if len(tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("buildTools: got %d function declarations, want 1", len(tools[0].FunctionDeclarations))
	}
	fd := tools[0].FunctionDeclarations[0]
	if fd.Name != "read_file" || fd.Description != "read a file" {
		t.Errorf("function declaration = %+v, want read_file/read a file", fd)
	}
	if fd.Parameters == nil {
		t.Fatal("function declaration Parameters = nil, want non-nil")
	}
}

// TestGeminiBuildToolsNilWhenEmpty locks that no tools → nil (field omitted).
func TestGeminiBuildToolsNilWhenEmpty(t *testing.T) {
	g := NewGemini("key")
	if tools := g.buildTools(wire.Request{}); tools != nil {
		t.Errorf("buildTools(empty) = %+v, want nil", tools)
	}
}

// TestGeminiOverflowClassification locks the context-overflow classifier.
// RESOURCE_EXHAUSTED is Gemini's 429 rate-limit status, NOT overflow — the
// old classifier sent rate-limited turns into compaction instead of retry.
func TestGeminiOverflowClassification(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"googleapi: Error 429: RESOURCE_EXHAUSTED: quota exceeded", false},
		{"resource_exhausted: rate limit", false},
		{"prompt is too long: 2000000 tokens > 1000000 maximum", true},
		{"maximum context length exceeded", true},
		{"input token count exceeds the maximum allowed", true},
		{"some other error", false},
		{"", false},
	}
	for _, c := range cases {
		// isGeminiOverflow takes an error, not a string; wrap it.
		err := errString(c.err)
		if got := isGeminiOverflow(err); got != c.want {
			t.Errorf("isGeminiOverflow(%q) = %v, want %v", c.err, got, c.want)
		}
	}
}

// errString wraps a string as an error for testing classifiers.
type errString string

func (e errString) Error() string { return string(e) }

// The multi-turn tool replay that single-turn tests could never catch.
//
// Gemini 3.x issues a thoughtSignature alongside every functionCall and
// REQUIRES it echoed back when that call is replayed on the next turn. Without
// it the continuation is rejected outright:
//
//	400 INVALID_ARGUMENT: Function call is missing a thought_signature in
//	functionCall parts.
//
// That made every tool-using Gemini turn die on turn 2 — the whole model
// family, not one version. It went unnoticed because the capability checks
// that ran against Gemini were all SINGLE-turn (text, vision, PDF, thinking),
// and a first turn has no prior call to replay. Verified live on Vertex against
// gemini-3.8-flash and gemini-3.6-flash: identical failure without the
// signature, success with it.
func TestGeminiRoundTripsThoughtSignature(t *testing.T) {
	const sig = "AY89a1+xXXW4pP89"
	encoded := base64.StdEncoding.EncodeToString([]byte(sig))

	// DECODE: a functionCall part's signature is captured onto the block.
	g := NewGemini("key")
	decoded, ok := toolUseFromPart(&genai.Part{
		FunctionCall:     &genai.FunctionCall{ID: "tu_1", Name: "bash", Args: map[string]any{"cmd": "ls"}},
		ThoughtSignature: []byte(sig),
	})
	if !ok || decoded.Type != "tool_use" {
		t.Fatalf("decode produced %+v (ok=%v), want a tool_use block", decoded, ok)
	}
	blocks := []wire.Block{decoded}
	if blocks[0].Signature != encoded {
		t.Fatalf("decoded signature = %q, want the base64 of what Gemini issued", blocks[0].Signature)
	}

	// ENCODE: replaying that block sends the signature back, byte-identical.
	contents := g.buildContents(wire.Request{Messages: []wire.Message{
		{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "run ls"}}},
		{Role: "assistant", Blocks: []wire.Block{blocks[0]}},
		{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: "tu_1", Name: "bash", Content: "a.txt"}}},
	}})
	if len(contents) != 3 {
		t.Fatalf("buildContents produced %d contents, want 3", len(contents))
	}
	var replayed *genai.Part
	for _, p := range contents[1].Parts {
		if p.FunctionCall != nil {
			replayed = p
		}
	}
	if replayed == nil {
		t.Fatal("the replayed assistant turn carries no functionCall")
	}
	if string(replayed.ThoughtSignature) != sig {
		t.Fatalf("replayed signature = %q, want %q — a missing or altered one is a hard 400, "+
			"not a degraded response", replayed.ThoughtSignature, sig)
	}
}

// A tool_use block with no signature (one that came from another vendor, or a
// model that issued none) replays without inventing one.
func TestGeminiReplayWithoutSignatureSendsNone(t *testing.T) {
	g := NewGemini("key")
	contents := g.buildContents(wire.Request{Messages: []wire.Message{
		{Role: "assistant", Blocks: []wire.Block{
			{Type: "tool_use", ID: "tu_1", Name: "bash", Input: []byte(`{"cmd":"ls"}`)},
		}},
	}})
	for _, p := range contents[0].Parts {
		if p.FunctionCall != nil && len(p.ThoughtSignature) != 0 {
			t.Fatalf("invented a signature %q for a call that never had one", p.ThoughtSignature)
		}
	}
}
