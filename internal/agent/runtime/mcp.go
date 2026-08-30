package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/mcp"
	"github.com/memcode-ai/memcode/internal/wire"
)

// This file is the SESSION side of MCP: lifecycle (connect at session start, close at end),
// advertising MCP tools to the model, and routing+gating an MCP tool call. The protocol and
// connection management live in internal/mcp; risk policy stays here so MCP tools pass through
// the SAME gate as bash (a SELECT auto-runs, a DROP escalates, mutations are blocked in
// research mode) — an external server never gets a softer permission path than a shell command.

const mcpClientVersion = "0.1.0"

// connectMCP resolves the configured MCP servers across scopes and connects the trusted set:
// local/user servers (you added them) plus project servers you've APPROVED. Un-approved project
// servers from a checked-in .mcp.json are held back (Claude Code's security model) — in an
// interactive session they're queued for an approval prompt on the first turn (see
// reviewPendingMCP); headless they're simply skipped. interactive enables the OAuth browser flow
// (a headless run has no one to complete it). Failures are non-fatal.
func (s *Session) connectMCP(ctx context.Context, interactive bool) {
	approvals := mcp.LoadApprovals(s.root)
	connect := map[string]mcp.ServerConfig{}
	s.mcpPending = nil
	// Retain each connected server's RAW config: approvals and invocation grants hash the raw
	// entry (see internal/mcp approvals — env changes must not invalidate them, and resolved
	// secrets must not feed the hash), so the gate needs the raw config at call time. Only the
	// connect map is env-expanded.
	s.mcpConfigs = map[string]mcp.ServerConfig{}
	for _, ss := range mcp.Resolve(s.root) {
		if ss.Scope == mcp.ScopeProject && approvals.Status(ss.Name, ss.Config) != mcp.Approved {
			s.mcpPending = append(s.mcpPending, ss) // untrusted until approved
			continue
		}
		connect[ss.Name] = mcp.ExpandServer(ss.Config)
		s.mcpConfigs[ss.Name] = ss.Config
	}
	// Programmatically-set servers (currently: existing-Chrome, see
	// SetExtraMCPServers) are already trusted by the caller that set them —
	// no approval gate, same as a locally-configured server.
	for name, cfg := range s.extraMCPServers {
		connect[name] = mcp.ExpandServer(cfg)
		s.mcpConfigs[name] = cfg
	}
	s.mcpInteractive = interactive
	s.mcp = mcp.Connect(ctx, connect, mcp.Options{Version: mcpClientVersion, AllowOAuth: interactive})
	s.reportMCP()
	if len(s.mcpPending) > 0 && !interactive {
		s.printf("● mcp: %d project server(s) pending approval — review with `memcode mcp`\n", len(s.mcpPending))
	}
}

// reviewPendingMCP prompts (interactively) to approve each project-scoped server held back at
// connect time, persists the choice, and connects the approved ones into the live session. Runs
// once — it clears the pending list. No-op when nothing is pending.
func (s *Session) reviewPendingMCP(ctx context.Context) {
	if len(s.mcpPending) == 0 {
		return
	}
	pending := s.mcpPending
	s.mcpPending = nil
	// approve holds the RAW configs (grants hash the raw entry); connect the expanded ones.
	approve := map[string]mcp.ServerConfig{}
	connect := map[string]mcp.ServerConfig{}
	for _, ss := range pending {
		req := ApprovalRequest{
			Label:  "MCP server",
			Title:  ss.Name,
			Detail: "project-scoped (from .mcp.json): " + endpointOf(ss.Config),
			Risk:   "review",
		}
		d := s.askApproval(ctx, req)
		decision := mcp.Rejected
		if d.Allow {
			decision = mcp.Approved
			approve[ss.Name] = ss.Config
			connect[ss.Name] = mcp.ExpandServer(ss.Config)
		}
		_ = mcp.SaveApproval(s.root, ss.Name, ss.Config, decision)
	}
	if len(approve) == 0 {
		return
	}
	if s.mcpConfigs == nil {
		s.mcpConfigs = map[string]mcp.ServerConfig{}
	}
	for name, cfg := range approve {
		s.mcpConfigs[name] = cfg
	}
	if s.mcp == nil {
		s.mcp = mcp.Connect(ctx, connect, mcp.Options{Version: mcpClientVersion, AllowOAuth: s.mcpInteractive})
	} else {
		s.mcp.Add(ctx, connect, mcp.Options{Version: mcpClientVersion, AllowOAuth: s.mcpInteractive})
	}
	s.reportMCP()
}

// reportMCP surfaces any NEW connection errors (Add accumulates them) and a one-line tool count.
func (s *Session) reportMCP() {
	if s.mcp == nil {
		return
	}
	errs := s.mcp.Errors()
	for _, e := range errs[min(s.mcpErrsShown, len(errs)):] {
		s.printf("  ⚠ mcp: %v\n", e)
	}
	s.mcpErrsShown = len(errs)
	if n := len(s.mcp.Tools()); n > 0 {
		s.printf("● mcp: %d tool(s) across %d server(s)\n", n, s.mcpServerCount())
	}
}

func endpointOf(sc mcp.ServerConfig) string {
	if sc.URL != "" {
		return sc.URL
	}
	return strings.TrimSpace(sc.Command + " " + strings.Join(sc.Args, " "))
}

// closeMCP tears down all MCP connections. Safe when s.mcp is nil.
func (s *Session) closeMCP() { s.mcp.Close() }

func (s *Session) mcpServerCount() int {
	seen := map[string]bool{}
	for _, t := range s.mcp.Tools() {
		seen[t.Server] = true
	}
	return len(seen)
}

// mcpToolDefs advertises MCP to the model — by progressive disclosure, ALWAYS. The connected
// tools' schemas are never inlined (a fat server would burn tens of KB per request, and any
// catalog change would invalidate the cached tools prefix): the model gets ONE meta-tool
// whose bytes are constant no matter what's connected — search to find tools, schema to read
// one, call to invoke — plus the per-server index line in the volatile facts. So the tools
// block only ever changes when MCP appears or disappears entirely, never per server or tool.
func (s *Session) mcpToolDefs() []wire.ToolDef {
	if s.mcp == nil {
		return nil
	}
	var out []wire.ToolDef
	if len(s.mcp.Tools()) > 0 {
		out = append(out, wire.ToolDef{
			Name:        tools.MCP,
			Description: "Use the user's connected MCP servers (external services — every call is gated; your facts list the servers). action:\"search\" lists tools matching `query` as name — description (empty query lists all). action:\"schema\" returns one tool's input schema — read it before your first call to that tool. action:\"call\" invokes `tool` with `args`. For loops or multi-call MCP workflows prefer mcp_code_exec, where search_tools(query), tool_schema(name), and mcp(tool, **args) are available to scripts and intermediates stay out of the conversation.",
			InputSchema: map[string]any{
				"type": "object", "required": []string{"action"},
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "enum": []string{"search", "schema", "call"}, "description": "search, schema, or call"},
					"query":  map[string]any{"type": "string", "description": "search: filter over tool names/descriptions (optional)"},
					"tool":   map[string]any{"type": "string", "description": "schema/call: the tool name exactly as search returned it"},
					"args":   map[string]any{"type": "object", "description": "call: the tool's arguments per its schema"},
				},
			},
		})
	}
	// Advertise the resource/prompt meta-tools ONLY when a connected server actually
	// exposes those capabilities — no empty affordance on a tools-only server.
	if len(s.mcp.Resources()) > 0 {
		out = append(out, wire.ToolDef{
			Name:        tools.MCPResource,
			Description: "[MCP] Access external resources servers expose (docs, schemas, runbooks). action:\"list\" to see them, action:\"read\" with a uri to fetch one's contents.",
			InputSchema: map[string]any{
				"type": "object", "required": []string{"action"},
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "enum": []string{"list", "read"}, "description": "list or read"},
					"uri":    map[string]any{"type": "string", "description": "resource uri (required for read)"},
				},
			},
		})
	}
	if len(s.mcp.Prompts()) > 0 {
		out = append(out, wire.ToolDef{
			Name:        tools.MCPPrompt,
			Description: "[MCP] Use reusable prompt templates servers expose. action:\"list\" to see them, action:\"get\" with a name (and args) to render one.",
			InputSchema: map[string]any{
				"type": "object", "required": []string{"action"},
				"properties": map[string]any{
					"action": map[string]any{"type": "string", "enum": []string{"list", "get"}, "description": "list or get"},
					"name":   map[string]any{"type": "string", "description": "prompt name (required for get)"},
					"args":   map[string]any{"type": "object", "description": "template arguments for get"},
				},
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// mcpTool is the meta-tool's dispatcher: search and schema are ungated catalog metadata
// (discovery is deliberately independent of authorization); call routes through invokeMCP.
func (s *Session) mcpTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.MCPInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	switch in.Action {
	case "search":
		out := s.mcpSearch(in.Query)
		s.toolLine(true, "MCP", "search "+clip(in.Query, 40), "", false)
		return textResult(out)
	case "schema":
		if strings.TrimSpace(in.Tool) == "" {
			return errResult("mcp schema needs a `tool`.")
		}
		s.toolLine(true, "MCP", "schema "+in.Tool, "", false)
		return textResult(s.mcpSchema(in.Tool))
	case "call":
		if strings.TrimSpace(in.Tool) == "" {
			return errResult("mcp call needs a `tool`.")
		}
		return s.invokeMCP(ctx, mcpOriginDirect, in.Tool, in.Args, nil)
	default:
		return errResult("mcp action must be \"search\", \"schema\", or \"call\".")
	}
}

// mcpResourceTool lists or reads MCP resources (read-only external context).
func (s *Session) mcpResourceTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.MCPResourceInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	switch in.Action {
	case "list":
		rs := s.mcp.Resources()
		if len(rs) == 0 {
			return textResult("no MCP resources available.")
		}
		var b strings.Builder
		for _, r := range rs {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", r.URI, r.Name, r.Description)
		}
		s.toolLine(true, "MCP", "resources", fmt.Sprintf("%d", len(rs)), false)
		return textResult(strings.TrimRight(b.String(), "\n"))
	case "read":
		if strings.TrimSpace(in.URI) == "" {
			return errResult("mcp_resource read needs a `uri`.")
		}
		out, err := s.mcp.ReadResource(ctx, in.URI)
		if err != nil {
			return errResult("mcp_resource read: " + err.Error())
		}
		s.toolLine(true, "MCP", "read "+in.URI, "", false)
		return textResult(s.redactor.Redact(truncate(out, maxToolOutput)))
	default:
		return errResult("mcp_resource action must be \"list\" or \"read\".")
	}
}

// mcpPromptTool lists or renders MCP prompt templates.
func (s *Session) mcpPromptTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.MCPPromptInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	switch in.Action {
	case "list":
		ps := s.mcp.Prompts()
		if len(ps) == 0 {
			return textResult("no MCP prompts available.")
		}
		var b strings.Builder
		for _, p := range ps {
			fmt.Fprintf(&b, "%s\t%s", p.Name, p.Description)
			for _, a := range p.Arguments {
				req := ""
				if a.Required {
					req = "*"
				}
				fmt.Fprintf(&b, "\t%s%s", a.Name, req)
			}
			b.WriteByte('\n')
		}
		s.toolLine(true, "MCP", "prompts", fmt.Sprintf("%d", len(ps)), false)
		return textResult(strings.TrimRight(b.String(), "\n"))
	case "get":
		if strings.TrimSpace(in.Name) == "" {
			return errResult("mcp_prompt get needs a `name`.")
		}
		out, err := s.mcp.GetPrompt(ctx, in.Name, in.Args)
		if err != nil {
			return errResult("mcp_prompt get: " + err.Error())
		}
		s.toolLine(true, "MCP", "prompt "+in.Name, "", false)
		return textResult(s.redactor.Redact(truncate(out, maxToolOutput)))
	default:
		return errResult("mcp_prompt action must be \"list\" or \"get\".")
	}
}

// mcpOrigin names the surface a call came from: the model's direct meta-tool call, or a
// mcp_code_exec script over the bridge. Both route through invokeMCP — one choke point, so
// neither surface can reach a server through a softer gate than the other.
type mcpOrigin string

const (
	mcpOriginDirect mcpOrigin = "direct"
	mcpOriginBridge mcpOrigin = "bridge"
)

// mcpRunGrants is one mcp_code_exec run's in-memory answer cache: an Execute covers that tool
// for the rest of the script and a Cancel auto-denies it, so a 50-call loop prompts once
// either way. callTime accumulates the run's remote TRANSPORT time for the MCP budget —
// deliberately excluding gate/prompt time, so a slow human answer costs the script nothing.
// Dies with the run — nothing here persists.
type mcpRunGrants struct {
	mu       sync.Mutex
	allowed  map[string]bool
	denied   map[string]bool
	callTime time.Duration
}

func newMCPRunGrants() *mcpRunGrants {
	return &mcpRunGrants{allowed: map[string]bool{}, denied: map[string]bool{}}
}

// invokeMCP resolves, gates, invokes, and audits one MCP tool call. The gate is the USER's,
// not a classifier's: connecting a server only lets it advertise tools; the first call to a
// tool prompts (Execute / Execute and remember / Don't ask again for <server> / Cancel) and
// remembered choices persist per project, keyed to the server's config hash. There are no
// verb heuristics and no annotation trust — nothing can be known about an opaque remote
// tool's semantics, so nothing pretends to; the deterministic rails around the gate (budgets,
// caps, per-call audit) are the guarantees. run carries a script run's grant cache (nil on
// the direct path).
func (s *Session) invokeMCP(ctx context.Context, origin mcpOrigin, name string, input json.RawMessage, run *mcpRunGrants) toolResult {
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return errResult("mcp tool: malformed input: " + err.Error())
		}
	}
	t, ok := s.mcp.Lookup(name)
	if !ok {
		return errResult("unknown MCP tool: " + name + " — discover tools with mcp{action:\"search\"}")
	}
	// Read-only explorers can't prompt (no HITL) and never egress on their own; the tools
	// aren't advertised to them, so this is a defensive backstop, not policy.
	if s.readOnly {
		return errResult("denied: read-only explorers can't call MCP tools")
	}
	if allowed, reason := s.gateMCP(ctx, t, args, run); !allowed {
		return errResult("mcp call denied: " + reason)
	}
	s.toolLine(true, "MCP", name, "", false)
	start := time.Now()
	out, isErr, err := s.mcp.Call(ctx, name, args)
	if run != nil {
		run.mu.Lock()
		run.callTime += time.Since(start)
		run.mu.Unlock()
	}
	s.auditMCP(ctx, origin, t, args, len(out), isErr, err)
	if err != nil {
		s.toolResult([]string{"failed: " + err.Error()})
		return errResult("mcp call failed: " + err.Error())
	}
	s.toolResult(mcpResultPreview(out, isErr))
	return toolResult{blocks: []wire.Block{wire.TextBlock(out)}, isError: isErr}
}

// gateMCP decides one invocation. Order: the secret rail trumps everything (a known secret
// value in the args prompts EVERY time, any mode, any grant — approving an MCP call approves
// egress of its arguments, and a known secret never auto-sends); then allow-all auto-runs
// (the user's explicit "stop asking"); then run-scoped answers, then persisted grants; else
// prompt. Plan mode reaches the prompt like any ask-mode call — with the classifier gone
// there is no "may mutate" guess to hard-deny on, so the user decides in the moment.
func (s *Session) gateMCP(ctx context.Context, t mcp.Tool, args map[string]any, run *mcpRunGrants) (bool, string) {
	secret := s.argsCarrySecret(args)
	if !secret {
		if s.effectiveMode() == permissions.ModeAllowAll {
			return true, ""
		}
		if run != nil {
			run.mu.Lock()
			allowed, denied := run.allowed[t.Name], run.denied[t.Name]
			run.mu.Unlock()
			if denied {
				return false, "denied earlier this run"
			}
			if allowed {
				return true, ""
			}
		}
		if cfg, ok := s.mcpConfigs[t.Server]; ok && mcp.LoadApprovals(s.root).CallAllowed(t.Server, cfg, t.Raw) {
			if run != nil {
				run.mu.Lock()
				run.allowed[t.Name] = true
				run.mu.Unlock()
			}
			return true, ""
		}
	}
	req := ApprovalRequest{
		Label: "MCP tool", Title: t.Name, Detail: mcpArgsPreview(args), Risk: "external",
		RememberScopes: []ApprovalScope{
			{Key: "tool", Label: "Execute and remember " + t.Raw},
			{Key: "server", Label: "Don't ask again for " + t.Server},
		},
	}
	if secret {
		// No remember scopes on a secret-carrying call: this approval is for THIS payload.
		req.RememberScopes = nil
		req.Detail = "⚠ args contain a known secret · " + req.Detail
	}
	d := s.askApproval(ctx, req)
	if !d.Allow {
		if run != nil && !secret {
			run.mu.Lock()
			run.denied[t.Name] = true
			run.mu.Unlock()
		}
		return false, orEmpty(d.Reason, "denied by user")
	}
	if cfg, ok := s.mcpConfigs[t.Server]; ok && d.RememberScope != "" {
		tool := t.Raw
		if d.RememberScope == "server" {
			tool = ""
		}
		if err := mcp.RememberCalls(s.root, t.Server, cfg, tool); err != nil {
			s.printf("  ⚠ mcp: couldn't persist grant: %v\n", err)
		}
	}
	if run != nil && !secret {
		run.mu.Lock()
		run.allowed[t.Name] = true
		run.mu.Unlock()
	}
	return true, ""
}

// argsCarrySecret reports whether the serialized args contain a value the redactor knows.
// Deterministic string matching, not classification — and defense in depth only: it cannot
// see encoded, transformed, or unregistered secrets.
func (s *Session) argsCarrySecret(args map[string]any) bool {
	if len(args) == 0 {
		return false
	}
	b, err := json.Marshal(args)
	if err != nil {
		return false
	}
	return s.redactor.Redact(string(b)) != string(b)
}

// auditMCP writes the per-invocation trail: which server, which tool, redacted args, sizes,
// outcome, origin. The audit is one of the deterministic guarantees the gate design leans on.
func (s *Session) auditMCP(ctx context.Context, origin mcpOrigin, t mcp.Tool, args map[string]any, outBytes int, isErr bool, err error) {
	outcome := "ok"
	switch {
	case err != nil:
		outcome = "transport-error: " + err.Error()
	case isErr:
		outcome = "tool-error"
	}
	s.emit(ctx, events.KindToolCalled, map[string]any{
		"tool":   t.Name,
		"server": t.Server,
		"origin": "mcp-" + string(origin),
		"input":  s.redactor.Redact(mcpArgsPreview(args)),
		"bytes":  outBytes,
		"result": outcome,
	})
}

// mcpArgsPreview renders a compact one-line preview of the call arguments for the approval card.
func mcpArgsPreview(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(b)
}

// mcpResultPreview is the short ⎿ block shown after a call: first non-empty line, marked when
// the tool reported an error.
func mcpResultPreview(out string, isErr bool) []string {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		line = "(no output)"
	}
	if isErr {
		line = "error: " + line
	}
	return []string{line}
}
