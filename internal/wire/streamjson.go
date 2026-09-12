package wire

import "encoding/json"

// Stream-json is the control protocol the sdk/agent wrapper speaks to a
// `memcode run --protocol stream-json` subprocess — the same shape as Anthropic's
// Claude Agent SDK driving the `claude` binary. It is versioned, newline-delimited
// JSON: ONE Envelope per line on stdout for machine events, stderr for human
// diagnostics. Every message carries correlation ids (turn/session) so a multi-turn
// session and its tool/permission round-trips stay unambiguous.

// StreamJSONVersion is the protocol version stamped on every Envelope. Bump on a
// breaking change to the message families or payloads.
const StreamJSONVersion = "1"

// Envelope is the single wire frame. Data is a type-specific payload (below).
type Envelope struct {
	Version string          `json:"version"`
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`      // correlation id (e.g. a permission/ask request↔response)
	TurnID  string          `json:"turn_id,omitempty"` // the turn this message belongs to
	Data    json.RawMessage `json:"data,omitempty"`
}

// Message families. The direction comments are from the wrapper's point of view.
const (
	// client → CLI (control)
	MsgInitialize         = "initialize"          // first message; configure the session
	MsgUserTurn           = "user_turn"           // submit a user prompt (starts a turn)
	MsgPermissionResponse = "permission_response" // answer a permission_request
	MsgAskResponse        = "ask_response"        // answer an ask_request
	MsgCancel             = "cancel"              // interrupt the running turn

	// CLI → client (events)
	MsgInitialized       = "initialized"        // session ready; carries session id
	MsgAssistantDelta    = "assistant_delta"    // SEMANTIC assistant text — what the model said
	MsgDisplay           = "display"            // rendered terminal output (ANSI, tool chrome) — presentation only
	MsgToolCall          = "tool_call"          // a tool is running
	MsgToolResult        = "tool_result"        // a tool finished (summary)
	MsgPermissionRequest = "permission_request" // the turn is blocked awaiting approval
	MsgAskRequest        = "ask_request"        // the turn is blocked awaiting a user choice
	MsgSessionState      = "session_state"      // busy/idle + room/mode telemetry
	MsgUsage             = "usage"              // running token counts
	MsgResult            = "result"             // a turn finished
	MsgError             = "error"              // a turn errored
)

// --- payloads ---

// InitializeData configures the session (client → CLI).
type InitializeData struct {
	Cwd  string `json:"cwd,omitempty"`  // working directory / repo root
	Mode string `json:"mode,omitempty"` // permission mode: ask | auto | allow-all
	Pin  string `json:"pin,omitempty"`  // the session's model label
}

// InitializedData announces a ready session (CLI → client).
type InitializedData struct {
	SessionID string `json:"session_id"`
	Protocol  string `json:"protocol"` // echoes the version for handshake validation
}

// UserTurnData submits a prompt (client → CLI).
type UserTurnData struct {
	Text string `json:"text"`
}

// AssistantDeltaData is assistant-generated text: what the MODEL said, and
// nothing else (CLI → client).
//
// Never rendering. A consumer must not have to strip ANSI, tool chrome, routing
// lines or spinners to discover the assistant's words — that is what DisplayData
// is for. These were one channel until 2026-09-12, and the reviewer that
// consumed it silently received a styled terminal transcript in place of the
// model's answer whenever a turn ended on a tool call.
type AssistantDeltaData struct {
	Text string `json:"text"`
}

// DisplayData is rendered terminal output: ANSI styling, tool-call chrome,
// routing lines, banners (CLI → client).
//
// For clients that want to SHOW what the terminal would show. Anything deciding
// what the model actually said wants AssistantDeltaData, or the final text on
// ResultData.
type DisplayData struct {
	Text string `json:"text"`
}

// ToolCallData / ToolResultData report tool activity (CLI → client).
type ToolCallData struct {
	Name   string `json:"name"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail,omitempty"`
}
type ToolResultData struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"` // "ok" | "failed" | …
}

// PermissionRequestData asks the client to approve an action (CLI → client). The
// client answers with a PermissionResponseData carrying the same Envelope.ID.
type PermissionRequestData struct {
	Title    string `json:"title"`
	Label    string `json:"label,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Command  string `json:"command,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Risk     string `json:"risk,omitempty"`
	Editable bool   `json:"editable,omitempty"`
}
type PermissionResponseData struct {
	Allow     bool   `json:"allow"`
	Command   string `json:"command,omitempty"`   // run this edited command instead
	Reason    string `json:"reason,omitempty"`    // when !allow: fed back to the model
	Interrupt bool   `json:"interrupt,omitempty"` // stop the whole turn
}

// AskRequestData / AskResponseData are the ask_user human-in-the-loop round-trip.
type AskRequestData struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}
type AskResponseData struct {
	Answer string `json:"answer"`
}

// UsageData reports running token counts (CLI → client).
type UsageData struct {
	OutputTokens int `json:"output_tokens"`
}

// SessionStateData reports busy/idle + light room telemetry (CLI → client).
type SessionStateData struct {
	Busy bool   `json:"busy"`
	Mode string `json:"mode,omitempty"` // room mode (normal/repair/replan), when known
}

// ResultData ends a turn (CLI → client).
//
// Text is the turn's final SEMANTIC assistant text, derived from what the model
// said and never from rendered output. It is legitimately empty when a turn
// ended without the model saying anything — every iteration was a tool call, or
// it hit its cap. An empty Text means exactly that, and a consumer must treat it
// as "no answer" rather than falling back to whatever else it captured.
type ResultData struct {
	Text      string `json:"text,omitempty"`
	Completed bool   `json:"completed"`
	// Spoke reports whether the model produced any text at all this turn. It
	// distinguishes "finished with nothing to say" from "finished and the text
	// went missing", which an empty string alone cannot.
	Spoke bool `json:"spoke"`
}

// ErrorData reports a turn-level error (CLI → client).
type ErrorData struct {
	Message string `json:"message"`
}
