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
	// Triggers are machine identifiers (a hostname, a URL) whose presence in a
	// request turns the tool on before the model is called, saving a turn.
	// Natural language is never a trigger; see the catalog note below.
	Triggers []string
	// Guide is told to the model the moment the capability turns on: how to
	// use it well, at no cost to the requests that never need it.
	Guide string
}

// Capability tool name: the tiny always-on tool that enables a deferred one.
const CapabilityTool = "capability"

// Capabilities is the catalog, in index order.
//
// Summary is the model's ROUTER line: what the capability is for and what it
// is not. The main model reading this index is the classifier; the summaries
// are where the understanding of what users mean belongs.
//
// Triggers are a latency shortcut for MACHINE identifiers only (a product
// hostname, a product URL), never natural language: approximating intent with
// a growing list of words and phrases misroutes ordinary requests, and a false
// trigger puts an irrelevant schema in front of the model. A capability with
// no identifier has no triggers and is reached through the index.
var Capabilities = []Capability{
	{Tool: Websites,
		Summary:  "memcode websites, a separate product: sites made in ~/.memcode/websites and hosted at {slug}.memcode.app (create, change, push, publish, manage). \"Make me a website\" usually is NOT this: it is ordinary development in the user's project with the file tools. Use this only when the user names memcode websites or memcode.app, is working on one they already have, or says so when asked; if the conversation doesn't settle which they mean, ask before assuming. Changing a memcode website or judging how it looks is always websites develop (its worker sees pages in a browser; you can't). For public sites elsewhere use fetch/web_search",
		Triggers: []string{"memcode.app"},
		Guide: "Every change to a memcode website, and every question about how one looks (\"make it like ode.com\", \"does it match?\", \"pick up where we left off\"), goes through websites develop. " +
			"Its worker opens the site and any reference in a real browser, measures and compares them, edits, and checks the production build. " +
			"You can't see pages, so never judge a site's look yourself, and never read, edit, serve or install packages in a site's folder with your own tools. " +
			"Pass the user's whole brief as prompt and any reference sites as references."},
	{Tool: Task,
		Summary: "work memcode should do LATER or REPEATEDLY on its own, without the user (\"every morning…\", \"each Monday…\", \"nightly…\"): scheduled automations to create, list, pause, delete. A request to do it once, now, is not this",
	},
	{Tool: Script,
		Summary: "command sequences the user SAVED earlier and wants replayed (\"the deploy recipe I saved\", \"run my check script\"), or a new one to save: save, find, run, delete. Not for writing a script file in the repo",
	},
	{Tool: Artifact,
		Summary:  "publish a local self-contained HTML file as a shareable memcode.ai page (publish, update, list, delete)",
		Triggers: []string{"memcode.ai/code/artifact"}},
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
