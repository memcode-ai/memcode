package tools

import "github.com/memcode-ai/memcode/internal/wire"

// Deferred capabilities: typed tools that are NOT in the default tool list.
// Each one's schema costs every request, so a tool whose need can be inferred
// before its first call (the user asks about websites, a schedule, a saved
// recipe, a page to publish) is advertised as a one-line index entry instead,
// and its typed tool is turned on for the rest of the session when the need
// shows up. Tools the model may need opportunistically while doing something
// else (preference_signal, todo, the file tools) stay always-on.
//
// This catalog is the TRUSTED activation map. A capability is enabled only by
// its ID here, from program code: a skill (built-in, repo or user) is
// instructions and nothing more, and can never turn a tool on by naming
// itself after one.
//
// Enabling is session-scoped and monotonic: once on, a tool stays on, so the
// tool list changes at most once per capability (one prompt-cache miss, never
// a tool that appears and disappears between turns).

// Capability is one deferred tool.
type Capability struct {
	// Tool is the typed tool this capability turns on; also its ID.
	Tool string
	// Summary is the index line: what it does, in the user's words.
	Summary string
	// Triggers are words in a request that turn the tool on before the model
	// is called, so a plain "what websites do I have?" never costs an extra
	// model turn. Over-matching costs one schema; under-matching falls back to
	// the model enabling it from the index.
	Triggers []string
}

// Capability tool name: the tiny always-on tool that enables a deferred one.
const CapabilityTool = "capability"

// Capabilities is the catalog, in index order.
var Capabilities = []Capability{
	{Tool: Websites, Summary: "AI-built websites hosted at memcode.ai: list, create, build, preview, publish, domains, assets",
		// Not "site": "call site" is everyday code vocabulary.
		Triggers: []string{"website", "websites", "memcode.app"}},
	{Tool: Task, Summary: "scheduled automations memcode runs without the user: create, list, pause, delete",
		Triggers: []string{"schedule", "scheduled", "schedules", "automate", "automation", "automations", "cron", "recurring", "nightly", "weekly", "daily"}},
	{Tool: Script, Summary: "saved multi-step command recipes, replayed by name: save, find, run, delete",
		Triggers: []string{"script", "scripts", "recipe", "recipes"}},
	{Tool: Artifact, Summary: "publish a local self-contained HTML file as a shareable memcode.ai page",
		Triggers: []string{"artifact", "artifacts", "shareable", "share"}},
}

// IsDeferred reports whether name is a deferred capability's tool.
func IsDeferred(name string) bool {
	for _, c := range Capabilities {
		if c.Tool == name {
			return true
		}
	}
	return false
}

// CapabilityDef is the always-on tool that turns a deferred capability on.
// ids are the capabilities this session may enable (account gating and modes
// already applied), so the enum never names a tool the session can't use.
func CapabilityDef(ids []string) wire.ToolDef {
	return wire.ToolDef{
		Name: CapabilityTool,
		Description: "Turn on a capability that is not loaded by default (listed under AVAILABLE CAPABILITIES). " +
			"Its tool is available from your next step and stays on for the rest of the session. " +
			"Use it when the request needs one of them; don't guess its arguments before it is on.",
		InputSchema: obj(map[string]any{
			"name": map[string]any{"type": "string", "enum": ids, "description": "the capability to turn on"},
		}, "name"),
	}
}
