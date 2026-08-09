package runtime

import "strings"

// maxToolLeakRounds bounds the tool-call-leak self-heal — a runaway guard so a model that
// keeps leaking can't spin forever (it's intermittent in practice, so a couple retries
// usually lands a real call).
const maxToolLeakRounds = 3

// toolLeakNudge tells the model its tool call arrived as TEXT and to re-issue it for real.
// The model's leaked turn is shown to it (appended as the assistant turn) right before this,
// so it sees exactly what failed.
const toolLeakNudge = "Your previous turn's tool call came through as PLAIN TEXT, not an actual tool call — " +
	"I received the literal `<tool_call>…<arg_key>…<arg_value>…` markup in your message, so NOTHING was " +
	"executed. Re-issue that SAME call as a REAL tool call via the tool-calling mechanism; do NOT write the " +
	"`<tool_call>`/`<arg_key>`/`<arg_value>` tags as text. Make exactly the one call you intended."

// detectLeakedToolCall reports the tool name when an assistant turn carries a GLM-style
// tool-call envelope as PLAIN TEXT instead of a parsed tool_use — i.e. the text contains
// "<tool_call>NAME<arg_key>…". GLM-family models (4.5/4.6/5.x) emit tool calls in this XML
// envelope; when the provider/gateway doesn't translate it into structured tool_calls it
// leaks into the message content, the loop sees zero tool_uses, and the turn dead-ends.
// Returns "" when there's no leak signature. (Only consulted when there are no real tool_uses,
// so a model merely *discussing* the tags in prose is the only false-positive surface — rare,
// and the worst case is one wasted re-prompt.)
func detectLeakedToolCall(text string) string {
	i := strings.Index(text, "<tool_call>")
	if i < 0 {
		return ""
	}
	rest := text[i+len("<tool_call>"):]
	k := strings.Index(rest, "<arg_key>")
	if k < 0 {
		return "" // "<tool_call>" alone isn't the GLM arg envelope — not a confirmed leak
	}
	name := strings.TrimSpace(rest[:k])
	if name == "" || len(name) > 64 { // a sane tool name; guard against grabbing a huge span
		return "tool"
	}
	return name
}
