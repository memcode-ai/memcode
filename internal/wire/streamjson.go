package wire

import "encoding/json"

// Stream-json is the control protocol the sdk/agent wrapper speaks to a
// `memcode agent --protocol stream-json` subprocess — the same shape as Anthropic's
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
	MsgSetModel           = "set_model"           // change the model pin mid-session (the GUI model picker)

	// CLI → client (events)
	MsgInitialized       = "initialized"        // session ready; carries session id
	MsgAssistantDelta    = "assistant_delta"    // a chunk of assistant text
	MsgToolCall          = "tool_call"          // a tool is running
	MsgToolResult        = "tool_result"        // a tool finished (summary)
	MsgDiff              = "diff"               // a structured file change (path + unified diff)
	MsgTodos             = "todos"              // the current plan/checklist steps
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
	Cwd    string `json:"cwd,omitempty"`    // working directory / repo root
	Mode   string `json:"mode,omitempty"`   // permission mode: ask | auto | allow-all
	Pin    string `json:"pin,omitempty"`    // pinned model label ("" = Automatic)
	Resume string `json:"resume,omitempty"` // resume a prior session by id or unique prefix ("" = fresh)
}

// SetModelData changes the pinned model mid-session (client → CLI), so the GUI
// model picker takes effect on the running session, not just at startup.
type SetModelData struct {
	Pin string `json:"pin"` // model id/label ("" = Automatic)
}

// InitializedData announces a ready session (CLI → client).
type InitializedData struct {
	SessionID string `json:"session_id"`
	Protocol  string `json:"protocol"` // echoes the version for handshake validation
}

// Attachment is a local file surfaced to a turn as context. Path is required;
// Name/Mime are advisory. The runtime reads the file locally — nothing is
// uploaded. Same effect as an @path reference, but with real structure.
type Attachment struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
	Mime string `json:"mime,omitempty"`
}

// UserTurnData submits a prompt (client → CLI).
type UserTurnData struct {
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// AssistantDeltaData is a chunk of streamed assistant text (CLI → client).
type AssistantDeltaData struct {
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
	Detail string `json:"detail,omitempty"` // compact result note, e.g. "5 tools · glm-5p2"
	Output string `json:"output,omitempty"` // muted continuation/result preview
}

// DiffData is a structured file change (CLI → client). The client renders its
// own syntax-highlighted diff from the unified text rather than parsing ANSI.
type DiffData struct {
	Path     string `json:"path"`
	Language string `json:"language,omitempty"`
	Unified  string `json:"unified,omitempty"` // unified diff text, no color
	Added    int    `json:"added"`
	Removed  int    `json:"removed"`
	NewFile  bool   `json:"new_file,omitempty"`
}

// TodoItem / TodosData carry the current plan checklist (CLI → client).
type TodoItem struct {
	Text   string `json:"text"`
	Status string `json:"status"` // pending | in_progress | done
}
type TodosData struct {
	Items []TodoItem `json:"items"`
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
	Allow         bool   `json:"allow"`
	Command       string `json:"command,omitempty"`        // run this edited command instead
	Reason        string `json:"reason,omitempty"`         // when !allow: fed back to the model
	Interrupt     bool   `json:"interrupt,omitempty"`      // stop the whole turn
	Remember      bool   `json:"remember,omitempty"`       // persist an allow-rule ("don't ask again")
	RememberScope string `json:"remember_scope,omitempty"` // which offered scope to remember
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
	InputTokens       int    `json:"input_tokens,omitempty"`        // cumulative session input, when available
	OutputTokens      int    `json:"output_tokens"`                 // live per-turn output counter
	TotalOutputTokens int    `json:"total_output_tokens,omitempty"` // cumulative session output, when available
	CacheReadTokens   int    `json:"cache_read_tokens,omitempty"`   // cumulative cache-read tokens, when available
	CacheWriteTokens  int    `json:"cache_write_tokens,omitempty"`  // cumulative cache-write tokens, when available
	ContextTokens     int    `json:"context_tokens,omitempty"`      // latest main-call context size, when available
	ContextWindow     int    `json:"context_window,omitempty"`      // serving context window, when available
	Model             string `json:"model,omitempty"`               // display model, when available
	ReasoningEffort   string `json:"reasoning_effort,omitempty"`    // honest serving-model reasoning tier, when available
	ServedBy          string `json:"served_by,omitempty"`           // compact actual serving lane/model label, when available
	ServedByok        bool   `json:"served_byok,omitempty"`         // last main call used user's provider key
	RunningShells     int    `json:"running_shells,omitempty"`      // live background shell count
}

// SessionStateData reports busy/idle + light room telemetry (CLI → client).
type SessionStateData struct {
	Busy bool   `json:"busy"`
	Mode string `json:"mode,omitempty"` // room mode (normal/repair/replan), when known
}

// ResultData ends a turn (CLI → client).
type ResultData struct {
	Text      string `json:"text,omitempty"` // final assistant text, if captured
	Completed bool   `json:"completed"`
}

// ErrorData reports a turn-level error (CLI → client).
type ErrorData struct {
	Message string `json:"message"`
}
