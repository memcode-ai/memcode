package compat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// TestToolCallSignatureSurvivesTheHostedRoundTrip is the production half of the
// Gemini thought-signature bug. Fixing the Gemini ADAPTER was not enough: on the
// hosted path the CLI never touches that adapter, and the tool call crosses the
// wire in OpenAI-compat shape, which has no field for opaque per-call state. The
// signature was dropped in translation, so every replay 400'd anyway.
//
// This pins the whole path: stream delta → block → encoded back for replay.
func TestToolCallSignatureSurvivesTheHostedRoundTrip(t *testing.T) {
	const sig = "opaque-thought-signature-from-gemini"

	// Inbound: the gateway streams the call with its signature attached.
	a := newStreamAccum(true)
	fn := FunctionCall{Name: "ripgrep", Arguments: `{"pattern":"func"}`}
	a.apply(ChatChunk{Choices: []ChunkChoice{{Delta: Delta{
		ToolCalls: []ToolCallDelta{{Index: 0, ID: "call_1", Type: "function", Function: &fn, MemcodeSignature: sig}},
	}}}}, wire.StreamHandler{})
	resp := a.response()

	var use wire.Block
	for _, b := range resp.Blocks {
		if b.Type == "tool_use" {
			use = b
		}
	}
	if use.ID != "call_1" {
		t.Fatalf("no tool_use decoded from the stream: %+v", resp.Blocks)
	}
	if use.Signature != sig {
		t.Fatalf("signature lost decoding the stream: got %q want %q", use.Signature, sig)
	}

	// Outbound: replaying that block must carry the signature back, or Gemini
	// rejects the turn with "Function call is missing a thought_signature".
	msg, err := encodeAssistant(wire.Message{Role: "assistant", Blocks: []wire.Block{use}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].MemcodeSignature != sig {
		t.Fatalf("signature lost encoding the replay: %+v", msg.ToolCalls)
	}

	// And it must actually reach the wire under its namespaced key.
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"memcode_signature":"`+sig+`"`) {
		t.Fatalf("signature is not on the wire: %s", raw)
	}
}

// TestToolCallWithoutSignatureStaysClean: providers that issue no signature must
// not gain an empty field — omitempty keeps the standard shape standard, so a
// non-memcode OpenAI-compat server sees exactly what it expects.
func TestToolCallWithoutSignatureStaysClean(t *testing.T) {
	msg, err := encodeAssistant(wire.Message{Role: "assistant", Blocks: []wire.Block{
		{Type: "tool_use", ID: "call_1", Name: "ripgrep", Input: json.RawMessage(`{}`)},
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "memcode_signature") {
		t.Fatalf("empty signature must not appear on the wire: %s", raw)
	}
}
