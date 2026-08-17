// Toolsets are the named groups an agent's tool policy speaks in
// (agents.<name>.toolsets / disabled_toolsets in gateway.yaml) — the same
// mechanism Hermes uses. A policy entry may name a toolset or an individual
// tool; deny always wins over allow. These names are the public contract,
// documented at memcode.ai/docs/agents/tools — rename one only with the docs.
package tools

// toolsetDefs maps each toolset name to its member tools, in display order.
var toolsetOrder = []string{
	"files", "shell", "code", "web", "browser", "mcp",
	"memory", "skills", "delegation", "planning", "interaction",
}

var toolsetDefs = map[string][]string{
	// Reading and editing files in the project.
	"files": {ReadFile, ListDir, Glob, Ripgrep, GitDiff, EditFile, ApplyPatch},
	// Executing commands on the machine (each still risk-gated per action).
	"shell": {Bash, Script, RunTests, Trace},
	// Understanding code: navigation, diagnostics, structure.
	"code": {CodeQuery, CodeNav, Diagnostics, RepoMap},
	// The public internet: search, fetch, GitHub, publishing.
	"web": {WebSearch, Fetch, GitHub, Artifact},
	// Driving a real Chrome browser.
	"browser": {
		BrowserNavigate, BrowserClick, BrowserType, BrowserScreenshot,
		BrowserEval, BrowserText, BrowserScroll, BrowserPressKey, BrowserHover,
		BrowserSelect, BrowserBack, BrowserForward, BrowserConsole,
		BrowserNewTab, BrowserSwitchTab, BrowserCloseTab, BrowserListTabs,
	},
	// Connected MCP servers (and orchestrating them from code).
	"mcp": {MCP, MCPResource, MCPPrompt, MCPCodeExec},
	// The agent's own knowledge: memory, todos, baseline facts.
	"memory": {Memcode, Todo, Knowledge, PreferenceSignal},
	// Installed skills.
	"skills": {Skill},
	// Spawning sub-agents and background work.
	"delegation": {Explore, Dispatch, Agent, Reasoning},
	// Plan mode.
	"planning": {EnterPlan, CancelPlan, ExecutePlan, RecallPlan},
	// Talking to the human.
	"interaction": {AskUser},
}

// ToolsetNames returns the toolset names in display order.
func ToolsetNames() []string { return append([]string(nil), toolsetOrder...) }

// ToolsetMembers returns a toolset's tools (nil for an unknown name).
func ToolsetMembers(set string) []string {
	return append([]string(nil), toolsetDefs[set]...)
}

// knownTool reports whether name is a canonical tool name.
func knownTool(name string) bool {
	for _, members := range toolsetDefs {
		for _, m := range members {
			if m == name {
				return true
			}
		}
	}
	return false
}

// ValidPolicyEntry reports whether a policy list entry names a toolset or a tool.
func ValidPolicyEntry(name string) bool {
	_, isSet := toolsetDefs[name]
	return isSet || knownTool(name)
}

// Policy is a compiled tool policy: allow (empty = everything) minus deny.
// Entries may be toolset names or individual tool names; deny wins.
type Policy struct {
	allow map[string]bool // nil = allow all
	deny  map[string]bool
}

// NewPolicy compiles allow/deny lists into a Policy. Unknown entries are
// returned so callers can surface them instead of silently ignoring them.
func NewPolicy(allow, deny []string) (Policy, []string) {
	var unknown []string
	expand := func(entries []string) map[string]bool {
		if len(entries) == 0 {
			return nil
		}
		m := map[string]bool{}
		for _, e := range entries {
			if members, ok := toolsetDefs[e]; ok {
				for _, t := range members {
					m[t] = true
				}
			} else if knownTool(e) {
				m[e] = true
			} else {
				unknown = append(unknown, e)
			}
		}
		return m
	}
	return Policy{allow: expand(allow), deny: expand(deny)}, unknown
}

// Empty reports whether the policy restricts nothing.
func (p Policy) Empty() bool { return p.allow == nil && len(p.deny) == 0 }

// Allows reports whether the policy permits a tool. Tools outside the
// canonical registry (dynamic surfaces) follow the toolset of their family
// when named, else the allow-list default.
func (p Policy) Allows(name string) bool {
	if p.deny[name] {
		return false
	}
	if p.allow == nil {
		return true
	}
	return p.allow[name]
}
