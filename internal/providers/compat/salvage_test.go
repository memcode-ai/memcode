package compat

// Salvage-net tests ported with the engine consolidation.

import (
	"encoding/json"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

func TestSalvageToolCall(t *testing.T) {
	tools := []wire.ToolDef{{Name: "bash"}, {Name: "read_file"}}
	ok := func(text, wantName, wantArgs string) {
		t.Helper()
		b, _, hit := salvageToolCall(text, tools)
		if !hit || b.Name != wantName || string(b.Input) != wantArgs {
			t.Fatalf("salvage(%q) = %+v hit=%v", text, b, hit)
		}
	}
	miss := func(text string) {
		t.Helper()
		if _, _, hit := salvageToolCall(text, tools); hit {
			t.Fatalf("salvage(%q) should not convert", text)
		}
	}
	// the exact shape Qwen2.5-Coder emits
	ok("```json\n{\n  \"name\": \"bash\",\n  \"arguments\": {\n    \"command\": \"date +%s\"\n  }\n}\n```",
		"bash", "{\n    \"command\": \"date +%s\"\n  }")
	// the EXACT wire output captured from Qwen2.5-Coder-14B-AWQ (hermes parser
	// missed <tools>): must salvage to a bash tool_use.
	ok("<tools>\n{\n  \"name\": \"bash\",\n  \"arguments\": {\n    \"command\": \"echo hi\"\n  }\n}\n</tools>",
		"bash", "{\n    \"command\": \"echo hi\"\n  }")
	ok("<tool_call>{\"name\":\"read_file\",\"arguments\":{\"path\":\"x\"}}</tool_call>",
		"read_file", `{"path":"x"}`)
	// preamble prose + a fenced tool call (the code_query failure) must salvage
	ok("To locate it, I'll use the read_file function.\n```json\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"x\"}}\n```",
		"read_file", "{\"path\": \"x\"}")
	ok(`{"name":"read_file","arguments":{"path":"main.go"}}`, "read_file", `{"path":"main.go"}`)
	// NOTE: preamble + a fenced tool-call block DOES convert now — in an agent
	// context a fenced {name,arguments} naming a real tool is intent-to-call, not
	// explanation; the two are structurally identical, and destructive calls are
	// still gated by CLI permissions.
	ok("Here is how you'd call it:\n```json\n{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}\n```",
		"bash", `{"command":"ls"}`)
	miss("```json\n{\"name\":\"unknown_tool\",\"arguments\":{}}\n```") // not an offered tool
	miss("plain text answer")
	miss("I considered using bash but decided against it.") // no fenced call
	miss(`{"name":"bash","arguments":{}} trailing junk`)    // bare (unfenced) + junk stays strict

	// applySalvage end-to-end: text-only response becomes a tool_use turn
	resp := applySalvage(wire.Response{StopReason: "end_turn", Blocks: []wire.Block{wire.TextBlock("```json\n{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}\n```")}}, tools)
	if resp.StopReason != "tool_use" || len(resp.ToolUses()) != 1 {
		t.Fatalf("applySalvage = %+v", resp)
	}
	// structured tool_calls are never touched
	structured := wire.Response{StopReason: "tool_use", Blocks: []wire.Block{{Type: "tool_use", Name: "bash", Input: json.RawMessage("{}")}}}
	if got := applySalvage(structured, tools); len(got.ToolUses()) != 1 || got.ToolUses()[0].ID != "" || got.ToolOrigin != "structured_openai" {
		t.Fatalf("structured response must pass through untouched: %+v", got)
	}
}
