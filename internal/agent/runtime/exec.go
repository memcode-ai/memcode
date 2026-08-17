package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/edit"
	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/jobs"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/secrets"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/events"
	detachedjobs "github.com/memcode-ai/memcode/internal/jobs"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"

	"sync"

	"github.com/memcode-ai/memcode/internal/repofiles"
	"github.com/memcode-ai/memcode/internal/sandbox"
)

// toolResult is what a tool handler returns: structured content blocks (text
// and/or image) plus an error flag. Most tools return a single text block; the
// browser screenshot tool returns an image block (vision). The error flag maps
// onto the tool_result's is_error field so the model knows the call failed.
type toolResult struct {
	blocks  []wire.Block // content blocks (text, image, …) — the tool's output
	isError bool         // true → the tool_result carries is_error
}

// textResult is the common case: a single text block, no error.
func textResult(text string) toolResult {
	return toolResult{blocks: []wire.Block{wire.TextBlock(text)}}
}

// errResult is an error result: a single text block + is_error.
func errResult(text string) toolResult {
	return toolResult{blocks: []wire.Block{wire.TextBlock(text)}, isError: true}
}

// imageResult is a vision result: an image block (e.g. a browser screenshot).
func imageResult(mediaType string, data []byte) toolResult {
	return toolResult{blocks: []wire.Block{wire.ImageBlock(mediaType, data)}}
}

// mixedResult carries both text and image blocks (e.g. a screenshot + a caption).
func mixedResult(text string, mediaType string, data []byte) toolResult {
	return toolResult{blocks: []wire.Block{
		wire.TextBlock(text),
		wire.ImageBlock(mediaType, data),
	}}
}

// documentResult carries a caption plus a native document block (a read PDF).
// The model reads the file directly on the LLM call; for models without
// document input the resolver absorbs the turn to a capable tier.
func documentResult(text string, mediaType string, data []byte) toolResult {
	return toolResult{blocks: []wire.Block{
		wire.TextBlock(text),
		wire.DocumentBlock(mediaType, data),
	}}
}

// text returns the concatenated text of a toolResult's text blocks — for
// display/markers that only deal with text (the model gets the full blocks).
func (r toolResult) text() string {
	var b strings.Builder
	for _, bl := range r.blocks {
		if bl.Type == "text" && bl.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

// executeBatch runs all of a turn's tool calls and returns their results in the
// model's original order (tool_use[i] → tool_result[i]). Read-only calls run
// CONCURRENTLY (the model already emits several per turn — see isParallelSafe),
// capped by a semaphore; mutating calls run serially inline. Session state the
// concurrent calls touch (output, counters) is mutex-guarded; the store is
// already safe for concurrent use.
func (s *Session) executeBatch(ctx context.Context, uses []wire.Block) []wire.Block {
	results := make([]wire.Block, len(uses))
	result := func(i int, u wire.Block) {
		tr := s.execute(ctx, u)
		// Assemble the tool_result block: use ContentBlocks (structured) when the
		// handler returned blocks, else fall back to flat Content (backwards compat).
		rb := wire.Block{Type: "tool_result", ToolUseID: u.ID, IsError: tr.isError}
		if len(tr.blocks) > 0 {
			rb.ContentBlocks = tr.blocks
			// Also set Content for display paths that only read the flat string —
			// the text blocks' concatenated text, so markers/previews still work.
			rb.Content = tr.text()
		} else {
			rb.Content = ""
		}
		results[i] = rb
	}
	sem := make(chan struct{}, maxParallelTools)
	var wg sync.WaitGroup
	for i, u := range uses {
		// Honor an interrupt MID-BATCH: when the user denies one action and chooses to
		// stop/redirect, the remaining tool calls in the same assistant turn must NOT
		// run — otherwise denying edit #1 still silently applies edits #2, #3 (the
		// "I said no but it kept editing" bug). Every tool_use still needs a paired
		// tool_result, so the skipped ones get a benign one rather than being dropped.
		if s.turn.interrupted || s.turn.redirected || ctx.Err() != nil {
			skip := "skipped — you stopped this turn"
			if s.turn.redirected && !s.turn.interrupted {
				skip = "skipped — the user redirected; see their instruction on the denied action above"
			}
			results[i] = wire.Block{Type: "tool_result", ToolUseID: u.ID, Content: skip, IsError: true}
			continue
		}
		if !isParallelSafe(u.Name) {
			result(i, u) // mutating tool: serial, in place
			continue
		}
		wg.Add(1)
		go func(i int, u wire.Block) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result(i, u)
		}(i, u)
	}
	wg.Wait()
	return results
}

// isParallelSafe reports whether a tool has no side effects and can run
// concurrently with others in the same turn. Mutating tools (edit, bash, todo)
// and anything needing approval stay serial.
func isParallelSafe(name string) bool {
	switch name {
	case tools.ReadFile, tools.ListDir, tools.Glob, tools.Ripgrep, tools.CodeQuery, tools.RepoMap, tools.GitDiff, tools.Memcode, tools.Explore, tools.Trace, tools.Knowledge:
		return true
	case tools.BrowserScreenshot, tools.BrowserText, tools.BrowserConsole, tools.BrowserListTabs:
		return true // read-only browser tools — safe to run concurrently
		// NOT BrowserEval: it runs arbitrary JS, gates at Dangerous, and mutates — stays serial.
	}
	return false
}

// execute runs one proposed tool call under the permission gate, capturing
// events, and returns the result as structured content blocks plus an error flag.
func (s *Session) execute(ctx context.Context, u wire.Block) toolResult {
	s.mu.Lock()
	s.metrics.toolCalls++
	s.mu.Unlock()
	// Redact the tool input before it is persisted — the model may pass a secret
	// (e.g. a bearer token in a command).
	s.emit(ctx, events.KindToolCalled, map[string]any{"tool": u.Name, "input": s.redactor.Redact(string(u.Input))})
	tr := s.dispatch(ctx, u)
	if tr.isError {
		s.mu.Lock()
		s.metrics.toolErrors++
		s.mu.Unlock()
	}
	// Central guarantee: no known secret value ever reaches the model — redact
	// the text of every block (images carry no text).
	for i := range tr.blocks {
		if tr.blocks[i].Type == "text" {
			tr.blocks[i].Text = s.redactor.Redact(tr.blocks[i].Text)
		}
	}
	return tr
}

func (s *Session) dispatch(ctx context.Context, u wire.Block) toolResult {
	// A reader sub-agent has no mutating tools; reject them defensively even if one
	// is somehow proposed (they aren't advertised — see toolDefs). Bash is the
	// exception: it's the gated inspect shell (read-only commands run, mutating ones
	// are gated/rejected per-command in bash()).
	if (s.readOnly || s.planCtl.Planning()) && isMutatingTool(u.Name) && u.Name != tools.Bash {
		who := "read-only explorer"
		if s.planCtl.Planning() {
			who = "plan mode (research only — no edits until you execute)"
		}
		return errResult("denied: this is a " + who + " (no " + u.Name + ")")
	}
	// Tool policy: enforcement at execution too, so a hallucinated call to a
	// tool the policy hid fails closed instead of running.
	if !s.toolPolicy.Empty() && !s.toolPolicy.Allows(u.Name) {
		return errResult("denied: the tool " + u.Name + " is disabled by this agent's tool policy")
	}
	// explore fans out read-only sub-agents (chat + plan), but never nests: an
	// explorer can't spawn more explorers.
	if u.Name == tools.Explore && s.readOnly {
		return errResult("denied: an explorer can't fan out more explorers (no nesting) — investigate with read_file/ripgrep and report instead")
	}
	// Read-only explorers never egress; web is the executive session's call.
	if (u.Name == tools.WebSearch || u.Name == tools.Fetch || u.Name == tools.Artifact) && s.readOnly {
		return errResult("denied: web_search/fetch/artifact aren't available to read-only explorers; gather external facts in the main session")
	}
	// Explorers don't ask the user (only the top-level agent/planner is HITL).
	if s.readOnly && u.Name == tools.AskUser {
		return errResult("denied: a read-only explorer can't do that — investigate and report instead")
	}
	// Read-only gather telemetry (counts only — never blocks; the model decides when it's
	// read enough, and a re-read of changed state must always run).
	s.noteGather(u.Name, u.Input)
	switch u.Name {
	case tools.ReadFile:
		return s.readFile(u.Input)
	case tools.ListDir:
		return s.listDir(u.Input)
	case tools.Glob:
		return s.globFiles(ctx, u.Input)
	case tools.Ripgrep:
		return s.ripgrep(ctx, u.Input)
	case tools.CodeQuery:
		return s.codeQuery(ctx, u.Input)
	case tools.GitDiff:
		return s.gitDiff(ctx, u.Input)
	case tools.EditFile:
		return s.editFile(ctx, u.Input)
	case tools.ApplyPatch:
		return s.applyPatch(ctx, u.Input)
	case tools.Bash:
		return s.bash(ctx, u.Input)
	case tools.MCPCodeExec:
		return s.codeExec(ctx, u.Input)
	case tools.Memcode:
		return s.memcodeTool(ctx, u.Input)
	case tools.Todo:
		return s.todoTool(ctx, u.Input)
	case tools.Explore:
		return s.exploreTool(ctx, u.Input)
	case tools.WebSearch:
		return s.webSearchTool(ctx, u.Input)
	case tools.Fetch:
		return s.fetchTool(ctx, u.Input)
	case tools.Trace:
		return s.traceTool(ctx, u.Input)
	case tools.AskUser:
		return s.askUserTool(ctx, u.Input)
	case tools.Skill:
		return s.useSkill(ctx, u.Input)
	case tools.Script:
		return s.useScript(ctx, u.Input)
	case tools.Knowledge:
		return s.useKnowledge(u.Input)
	case tools.EnterPlan:
		return s.enterPlanTool(ctx, u.Input)
	case tools.CancelPlan:
		return s.cancelPlanTool(ctx, u.Input)
	case tools.Reasoning:
		return s.reasoningTool(ctx, u.Input)
	case tools.ExecutePlan:
		return s.executePlanTool(ctx, u.Input)
	case tools.Dispatch:
		return s.dispatchTool(ctx, u.Input)
	case tools.Agent:
		return s.agentTool(ctx, u.Input)
	case tools.RecallPlan:
		return s.recallPlanTool(ctx, u.Input)
	case tools.PreferenceSignal:
		return s.preferenceSignalTool(ctx, u.Input)
	case tools.BrowserNavigate:
		return s.browserNavigateTool(ctx, u.Input)
	case tools.BrowserClick:
		return s.browserClickTool(ctx, u.Input)
	case tools.BrowserType:
		return s.browserTypeTool(ctx, u.Input)
	case tools.BrowserScreenshot:
		return s.browserScreenshotTool(ctx, u.Input)
	case tools.BrowserEval:
		return s.browserEvalTool(ctx, u.Input)
	case tools.BrowserText:
		return s.browserTextTool(ctx, u.Input)
	case tools.BrowserScroll:
		return s.browserScrollTool(ctx, u.Input)
	case tools.BrowserPressKey:
		return s.browserPressKeyTool(ctx, u.Input)
	case tools.BrowserHover:
		return s.browserHoverTool(ctx, u.Input)
	case tools.BrowserSelect:
		return s.browserSelectTool(ctx, u.Input)
	case tools.BrowserBack:
		return s.browserBackTool(ctx, u.Input)
	case tools.BrowserForward:
		return s.browserForwardTool(ctx, u.Input)
	case tools.BrowserConsole:
		return s.browserConsoleTool(ctx, u.Input)
	case tools.BrowserNewTab:
		return s.browserNewTabTool(ctx, u.Input)
	case tools.BrowserSwitchTab:
		return s.browserSwitchTabTool(ctx, u.Input)
	case tools.BrowserCloseTab:
		return s.browserCloseTabTool(ctx, u.Input)
	case tools.BrowserWait:
		return s.browserWaitTool(ctx, u.Input)
	case tools.BrowserUpload:
		return s.browserUploadTool(ctx, u.Input)
	case tools.BrowserResize:
		return s.browserResizeTool(ctx, u.Input)
	case tools.BrowserListTabs:
		return s.browserListTabsTool(ctx, u.Input)
	case tools.MCP:
		return s.mcpTool(ctx, u.Input)
	case tools.MCPResource:
		return s.mcpResourceTool(ctx, u.Input)
	case tools.MCPPrompt:
		return s.mcpPromptTool(ctx, u.Input)
	case tools.GwOverview, tools.GwChannel, tools.GwPairing, tools.GwProject, tools.GwAgent, tools.GwSchedule, tools.GwService:
		return s.adminTool(ctx, u.Name, u.Input)
	case tools.GitHub:
		return s.githubTool(ctx, u.Input)
	case tools.RunTests:
		return s.runTestsTool(ctx, u.Input)
	case tools.Diagnostics:
		return s.diagnosticsTool(ctx, u.Input)
	case tools.CodeNav:
		return s.codeNavTool(ctx, u.Input)
	case tools.RepoMap:
		return s.repoMapTool(ctx, u.Input)
	case tools.Artifact:
		return s.artifactTool(ctx, u.Input)
	default:
		if s.mcp.Has(u.Name) { // a namespaced MCP tool name (compat alias for mcp{action:"call"})
			return s.invokeMCP(ctx, mcpOriginDirect, u.Name, u.Input, nil)
		}
		return errResult("unknown tool: " + u.Name)
	}
}

// enterPlanTool is the model's door into plan mode — the same flow /plan opens (research-only
// tools, the planner prompt, clarifying questions, then a proposed plan the USER approves). It
// transitions at this tool-execution boundary, so plan mode owns the NEXT turn cleanly. Idempotent;
// the human still owns approve/execute (the model gets no path to self-approve — see cancelPlanTool).
func (s *Session) enterPlanTool(ctx context.Context, in json.RawMessage) toolResult {
	if s.planCtl.Planning() {
		return textResult("already in plan mode — keep researching, ask any clarifying questions, then propose the plan for the user to approve.")
	}
	var p tools.EnterPlanInput
	if err := json.Unmarshal(in, &p); err != nil {
		return errResult("enter_plan: malformed input: " + err.Error())
	}
	opts := []PlanOpt{WithTask(s.lastUserText)}
	if p.Yolo {
		opts = append(opts, WithYolo())
	}
	s.EnterPlan(ctx, opts...)
	s.toolLine(true, "Plan", "", "entering plan mode", false)
	return textResult("Entered plan mode. Research the task with read-only tools, ASK the user clarifying questions on any decision-blocking fork, then present a concrete step-by-step plan. Do NOT edit or execute — the user approves the plan before anything runs.")
}

// cancelPlanTool CANCELS plan mode on the user's behalf (they changed their mind / want out). Cancel
// ONLY: it returns to normal chat and executes NOTHING. Approving-and-executing a plan is the user's
// explicit action (the approval selector), never the model's — so this hardwires approved=false and
// the model is never offered a self-approve path. The result text plans for the MISTAKEN call too
// (a model finishing research and reaching for "exit plan" out of Claude Code habit): it states the
// consequence bluntly and gives the recovery path, so the model can't narrate a plan that isn't there.
func (s *Session) cancelPlanTool(ctx context.Context, _ json.RawMessage) toolResult {
	if !s.planCtl.Planning() {
		return textResult("not in plan mode — nothing to cancel.")
	}
	s.ExitPlan(ctx, false) // approved=false: cancel, never execute
	s.toolLine(true, "Plan", "", "cancelled", false)
	return textResult("Plan mode CANCELLED — the plan was ABANDONED. Nothing was presented to the user and nothing will execute. There is NO plan on the table now, so do not describe one as ready. If the user did NOT ask you to stop planning, this call was a mistake: say so briefly, re-enter plan mode with enter_plan (your research is still in this conversation), and FINISH by writing the plan as your reply — prose, no tool call.")
}

// executePlanTool is the model's APPROVED-execute path: the user, after seeing the plan, told it
// to go. It flips the state machine into the apply phase (ExitPlan approved=true pins the plan as
// the contract and sets Applying) — runTurn then chains straight into execution. This is the fix
// for "typed execute didn't enter the execution phase": the transition is now an explicit,
// state-aware tool call instead of the gate silently treating the message as a plan revision. The
// human still decides (the tool fires only on their execute instruction); the model never
// self-approves on its own initiative (see the tool description), and the apply phase still
// prompts on catastrophic ops.
func (s *Session) executePlanTool(ctx context.Context, _ json.RawMessage) toolResult {
	if !s.planCtl.Planning() {
		return textResult("not in plan mode — there's no plan to execute.")
	}
	s.ExitPlan(ctx, true) // approved=true: pin the plan as the contract, enter the apply phase
	if !s.planCtl.IsApplying() {
		// ExitPlan only arms the apply when there's a plan to run; nothing was pinned.
		return errResult("no plan has been proposed yet — present the plan first, then execute once the user approves.")
	}
	s.toolLine(true, "Plan", "", "executing", false)
	return textResult("Plan approved — entering the execution phase now. Implement the plan's steps in order.")
}

// exploreTool spawns a read-only sub-agent to investigate one facet and returns
// its findings. Multiple explore calls in a turn run concurrently (explore is
// parallel-safe), each as its own read-only session whose tool calls/narration
// go to io.Discard — only this compact marker and the finding surface. This is
// the model-driven map/reduce: the model emits N explore() calls (map), each
// runs, and the model synthesizes the returned findings (reduce).
func (s *Session) exploreTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.ExploreInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return errResult("explore needs a `question`.")
	}
	if f := strings.TrimSpace(in.Focus); f != "" {
		q += " (focus on: " + f + ")"
	}
	// Explore is the read-only RESEARCH flavor of the one agent engine (spawnAgent): a cheap
	// scout (Tier Fast, Purpose Explore) whose spend is auto-attributed via the forked runner's
	// shared ledger. Multiple explore calls run concurrently (explore is parallel-safe).
	res, err := s.spawnAgent(ctx, AgentSpec{Task: q, Tier: TierFast, ReadOnly: true, Scope: in.Scope, Purpose: llm.Explore})
	// Claude-Code-style marker: ⏺ Explore(scope) with a status-colored bullet
	// (green done / red failed) and the question dimmed beneath.
	scope := in.Scope
	if scope == "" {
		scope = "repo"
	}
	if err != nil {
		s.toolLine(true, "Explore", scope, "failed", true)
		s.printf("%s\n", metaStyle.Render("  ⎿ failed: "+clip(err.Error(), 200)))
		return errResult("explore failed: " + err.Error())
	}
	status := fmt.Sprintf("%d tools", res.ToolCalls)
	if res.ServedBy != "" {
		status += " · " + res.ServedBy // who ran this scout — cheap lane (minimax) or an Anthropic fallback
	}
	s.toolLine(true, "Explore", scope, status, false)
	s.printf("%s\n", metaStyle.Render("   "+strings.TrimSpace(q)))
	return textResult(s.spillReport("explore-"+scope, s.redactor.Redact(res.Text)))
}

const (
	// reportSpillThreshold: a sub-agent report longer than this is written to
	// disk whole and returned as digest + read_file pointer. A 275-tool-call
	// explore whose report gets truncated in-conversation is the most expensive
	// possible way to produce nothing — the audited session ran the same explore
	// three times because its report didn't fit. On disk it participates in
	// range reads and eviction like any file.
	reportSpillThreshold = 16 * 1024
	reportSpillDigest    = 4 * 1024 // how much of the report stays inline as the digest
)

// spillReport persists an oversized sub-agent report under the session dir and
// returns a digest plus a read_file pointer to the full text. Under the
// threshold (or on any write failure) the report passes through unchanged —
// spilling is an optimization, never a failure mode. text must already be
// redacted (it is written to disk verbatim).
func (s *Session) spillReport(kind, text string) string {
	if len(text) <= reportSpillThreshold || s.sessionID == "" {
		return text
	}
	dir := filepath.Join(s.root, ".memcode", "sessions", s.sessionID, "reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return text
	}
	s.mu.Lock()
	s.metrics.reportsSpilled++
	seq := s.metrics.reportsSpilled
	s.mu.Unlock()
	name := fmt.Sprintf("%03d-%s.md", seq, sanitizeReportKind(kind))
	rel := filepath.Join(".memcode", "sessions", s.sessionID, "reports", name)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		return text
	}
	cut := reportSpillDigest
	if i := strings.LastIndexByte(text[:cut], '\n'); i > cut/2 {
		cut = i // break at a line boundary so the digest doesn't end mid-sentence
	}
	return text[:cut] + fmt.Sprintf(
		"\n\n[report continues — %s total. Full text saved to %s; read_file it (use start_line/end_line for sections).]",
		byteCount(len(text)), rel)
}

// sanitizeReportKind makes a report label filename-safe.
func sanitizeReportKind(kind string) string {
	kind = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '-'
	}, kind)
	if len(kind) > 40 {
		kind = kind[:40]
	}
	return kind
}

// agentTool is the first-class `agent` tool: spawn a sub-agent on a chosen tier (fast scout /
// strong Anthropic), read-only or mutating, run it to completion, and return its result to the
// calling model. This is the unified primitive — explore is its read-only-research sugar, and
// (Phase 2) background runs reuse it via the jobs registry.
func (s *Session) agentTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.AgentInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	task := strings.TrimSpace(in.Task)
	if task == "" {
		return errResult("agent needs a `task`.")
	}
	if c := strings.TrimSpace(in.Context); c != "" {
		task = "Context:\n" + c + "\n\nTask:\n" + task
	}
	tier := TierFast
	if strings.EqualFold(strings.TrimSpace(in.Tier), "strong") {
		tier = TierStrong
	}
	label := "fast"
	if tier == TierStrong {
		label = "strong"
	}
	// Background: run detached (reuse the jobs registry) and report the RESULT back to the LLM
	// when it finishes (report-back=true) — unlike fire-and-forget dispatch. The completion poll
	// feeds the persisted result into the engine as a new turn (see vxui agentDoneNotifications).
	// A long-running background agent runs unattended on a substantial task, so it uses the
	// FRONTIER (top strong) tier regardless of the requested fast/strong param.
	if in.Background {
		job, err := detachedjobs.Spawn(s.root, task, string(permissions.ModeAuto), "frontier", s.browserEnabled, true, "")
		if err != nil {
			s.toolLine(true, "Agent", clip(task, 60), "failed", true)
			return errResult("agent (background) failed to start: " + err.Error())
		}
		s.toolLine(true, "Agent", clip(task, 60), "background frontier "+job.ID, false)
		return textResult(fmt.Sprintf("started background agent %s (frontier tier, pid %d). It runs detached; "+
			"its RESULT will be delivered back to you as a new turn when it finishes — keep working in the "+
			"meantime, don't wait. Check /agents for status.", job.ID, job.PID))
	}
	res, err := s.spawnAgent(ctx, AgentSpec{Task: task, Tier: tier, ReadOnly: in.ReadOnly, Purpose: llm.Agent})
	if err != nil {
		s.toolLine(true, "Agent", clip(task, 60), "failed", true)
		s.printf("%s\n", metaStyle.Render("  ⎿ failed: "+clip(err.Error(), 200)))
		return errResult("agent failed: " + err.Error())
	}
	status := label + fmt.Sprintf(" · %d tools", res.ToolCalls)
	if res.ServedBy != "" {
		status += " · " + res.ServedBy
	}
	s.toolLine(true, "Agent", clip(task, 60), status, false)
	return textResult(s.spillReport("agent-"+label, s.redactor.Redact(res.Text)))
}

// dispatchTool offloads a discrete block of work to a hands-off background sub-agent.
// It reuses jobs.Spawn (the same detached-child primitive as `memcode run
// --background`): the sub-agent runs the full mutating agent loop as a separate OS
// process, serialized behind the repo writer lock, with NO prompts or clarifying
// questions (a headless child with stdin=nil auto-denies unapproved writes and
// auto-skips ask_user on EOF). Fire-and-forget: the tool returns the job id
// immediately and the main session keeps going. The TUI footer tracks the live
// agent count and surfaces a done-notification when the sub-agent finishes.
func (s *Session) dispatchTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.DispatchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	task := strings.TrimSpace(in.Task)
	if task == "" {
		return errResult("dispatch needs a `task`.")
	}
	mode := strings.TrimSpace(in.Mode)
	if mode == "" {
		mode = string(permissions.ModeAuto) // a backgrounded agent can't answer prompts
	} else if mode != string(permissions.ModeAuto) && mode != string(permissions.ModeAllowAll) {
		return errResult("dispatch `mode` must be \"auto\" or \"allow-all\" (got " + mode + ")")
	}
	// In --ask mode, dispatching an autonomous mutating agent is a bigger commitment than
	// a single edit — the sub-agent edits/runs commands FREELY with no further prompts. Gate
	// it behind explicit approval. This deliberately does NOT honor the session's
	// "don't ask again for edits" grant (editsAllowed): that covers individual edits, not
	// launching a whole sub-agent. Each dispatch prompts fresh in --ask.
	if s.effectiveMode() == permissions.ModeAsk {
		d := s.askApproval(ctx, ApprovalRequest{
			Title:  clip(task, 60),
			Label:  "Dispatch sub-agent",
			Detail: fmt.Sprintf("hands-off %s-mode agent that edits and runs commands without further prompts", mode),
			Risk:   permissions.Medium.String(),
		})
		if !d.Allow {
			s.toolLine(true, "Dispatch", clip(task, 60), "denied", true)
			return errResult("dispatch denied: " + orEmpty(d.Reason, "the user did not approve launching the sub-agent"))
		}
	}
	job, err := detachedjobs.Spawn(s.root, task, mode, "", s.browserEnabled, false, "")
	if err != nil {
		s.toolLine(true, "Dispatch", clip(task, 60), "failed", true)
		s.printf("%s\n", metaStyle.Render("  ⎿ failed: "+clip(err.Error(), 200)))
		return errResult("dispatch failed: " + err.Error())
	}
	s.toolLine(true, "Dispatch", clip(task, 60), fmt.Sprintf("started %s (pid %d)", job.ID, job.PID), false)
	return textResult(fmt.Sprintf("dispatched sub-agent %s (pid %d) — running hands-off in %s mode.\n"+
		"this is FIRE-AND-FORGET: the result goes to the USER (footer agent count + scrollback notification), NOT back to you as a tool result.\n"+
		"you will NOT learn whether the sub-agent succeeded, failed, or what it changed — so do not dispatch work you need to follow up on.\n"+
		"the user can check /agents or `memcode jobs logs %s`, or stop it with /agents stop %s.",
		job.ID, job.PID, mode, job.ID, job.ID))
}

// webSearchTool searches the web for a query via the provider's server-side web
// search. Marker: ⏺ Web Search(<query>).
func (s *Session) webSearchTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.WebSearchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return errResult("web_search needs a `query`.")
	}
	ws, ok := s.prov.(provider.WebSearcher)
	if !ok {
		s.toolLine(true, "Web Search", query, "unavailable", true)
		return errResult("web search is not available with the current provider.")
	}
	// Print the marker BEFORE the (multi-second, blocking) call so the card shows at
	// call time, not after the result lands.
	s.toolLine(true, "Web Search", query, "", false)
	ans, err := ws.WebSearch(ctx, query)
	if err != nil {
		// Surface the real error inline — don't swallow it as a bare "· failed".
		s.printf("%s\n", metaStyle.Render("  ⎿ failed: "+clip(err.Error(), 200)))
		return errResult("web search failed: " + err.Error())
	}
	return textResult(s.redactor.Redact(ans))
}

// bashPreview is the short ⎿ output shown under a Bash marker: a brief muted peek at
// stdout (then stderr), so a noisy command (find/grep/test) doesn't flood scrollback.
// "(no output)" when silent. See linesPreview for the clip/truncate rules.
func bashPreview(stdout, stderr string, exitCode int) []string {
	var src string
	switch {
	case stdout != "":
		src = stdout
	case stderr != "":
		src = stderr
	default:
		if exitCode == 0 {
			return []string{"(no output)"}
		}
		return []string{fmt.Sprintf("exit %d", exitCode)}
	}
	return linesPreview(src, 4)
}

// linesPreview clips text to at most maxLines rows for a muted ⎿ preview under a tool
// marker: at most maxLines rows, then a "+N lines" note, each row truncated to one line
// (long lines end in …, never wrapped). Shared by bashPreview and the diagnostics tools
// (diagResult/lspDiagResult) so a long diagnostic dump gets the same short, scannable peek
// a Bash command's output does — instead of a bare red "N line(s)" marker with the actual
// errors visible only to the model, never to the user watching scrollback.
func linesPreview(text string, maxLines int) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	out := lines
	if len(lines) > maxLines {
		out = append(append([]string{}, lines[:maxLines]...), fmt.Sprintf("… +%d lines", len(lines)-maxLines))
	}
	for i, l := range out {
		out[i] = clip(l, 100) // truncate long lines to one row (…), never wrap
	}
	return out
}

// abnormalExitReason returns a legible reason a command did NOT exit normally — the turn was
// cancelled (Esc/Ctrl-C), the bash timeout fired, or it never started — so the UI and the model
// see "interrupted" / "timed out after 2m0s" instead of a meaningless "exit -1" (a signal-kill's
// exit code). "" means it exited normally; use the real exit code. cctx.Err() returns the exact
// context sentinel, so direct comparison is correct (no errors.Is needed).
func abnormalExitReason(cctx context.Context, runErr error, exitCode int) string {
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		return fmt.Sprintf("timed out after %s", bashTimeout)
	case cctx.Err() == context.Canceled:
		return "interrupted"
	case exitCode < 0 && runErr != nil:
		return "could not run: " + runErr.Error()
	default:
		return ""
	}
}

// isMutatingTool reports whether a tool can change the repo or out-of-band
// state — these are withheld during read-only research (explorer / plan mode).
func isMutatingTool(name string) bool {
	switch name {
	case tools.EditFile, tools.ApplyPatch, tools.Bash, tools.Todo, tools.MCPCodeExec:
		return true
	}
	return false
}

// reviewTool is the strict read-only whitelist for the tooled plan reviewer — file
// reads, grep, listing, diff, and the deterministic code locator. Deliberately excludes
// bash (no shell execution) and every mutating/egress/sub-agent tool.
func reviewTool(name string) bool {
	switch name {
	case tools.ReadFile, tools.Ripgrep, tools.ListDir, tools.Glob, tools.GitDiff, tools.CodeQuery:
		return true
	}
	return false
}

// toolDefs returns the tools advertised to the model for the current mode.
func (s *Session) toolDefs() []wire.ToolDef {
	if s.adminMode {
		// Admin sessions get the admin registry plus a small file surface for
		// agent homes (instructions, memory, skills): read/edit/search/bash
		// and ask_user. No MCP, no browser, no repo/coding tools — config
		// changes go through the typed gw_* tools, which validate and
		// hot-reload.
		defs := tools.AdminDefs()
		adminExtra := map[string]bool{
			tools.AskUser: true, tools.ReadFile: true, tools.EditFile: true,
			tools.Bash: true, tools.Ripgrep: true, tools.Glob: true,
		}
		for _, d := range tools.Defs() {
			if adminExtra[d.Name] {
				defs = append(defs, d)
			}
		}
		return defs
	}
	defs := tools.Defs()
	out := make([]wire.ToolDef, 0, len(defs))
	for _, d := range defs {
		if s.allowTool(d.Name) {
			out = append(out, d)
		}
	}
	// Browser tools — only advertised when --chrome is set, and never to read-only
	// explorer sub-agents (the browser is an executive-session capability).
	if s.browserEnabled && !s.readOnly {
		for _, d := range tools.BrowserDefs() {
			if !s.allowTool(d.Name) {
				continue
			}
			out = append(out, d)
		}
	}
	// MCP tools go to the EXECUTIVE only (chat + plan), never to read-only explorer sub-agents
	// (like web/skill tools) — and each call is still gated per-tool in callMCP.
	if !s.readOnly {
		for _, d := range s.mcpToolDefs() {
			if !s.allowTool(d.Name) {
				continue
			}
			out = append(out, d)
		}
	}
	return out
}

// allowTool decides whether a tool is offered in the current mode.
//   - explore fans out read-only sub-agents. Offered in normal chat AND plan mode
//     (decompose a review/research task into parallel per-claim verifiers), but
//     NEVER nested inside an explorer (no sub-agents spawning sub-agents).
//   - explorers (read-only sub-agents): research tools + the bash inspect shell only.
//   - plan mode: research tools + bash + explore + ask_user + web_search/fetch.
//   - normal mode: everything, including explore.
func (s *Session) allowTool(name string) bool {
	// The agent's tool policy (agents.<name>.toolsets / disabled_toolsets) is
	// the outermost filter: a tool outside it is never advertised. The risk
	// gate still judges every action of the tools that remain.
	if !s.toolPolicy.Empty() && !s.toolPolicy.Allows(name) {
		return false
	}
	if name == tools.Explore {
		return !s.readOnly // chat + plan, but no nesting inside an explorer
	}
	// Web tools (search / fetch) are available to the executive session — the MODEL
	// judges when a question needs current/external facts (the prompt sets the
	// repo-first policy). Only read-only explorers are hard-denied, so sub-agents
	// never egress on their own. A brittle keyword pre-gate used to hide these and
	// produced "I don't have internet" on obviously web-shaped asks; the model is a
	// far better judge of intent than a regex.
	if name == tools.WebSearch {
		// Server-side search is a memcode gateway side channel; a custom endpoint
		// has no search backend, so the def isn't advertised at all off-gateway
		// (the artifact gate pattern: capability-driven, no vendor cases). fetch
		// stays — it's a LOCAL HTTP fetch that merely loses its server-side
		// PDF/extraction escalation on an endpoint.
		return !s.readOnly && !s.endpointMode()
	}
	if name == tools.Fetch || name == tools.Trace {
		return !s.readOnly
	}
	// A detached job (gateway/channel, background) has no approver: a Dangerous-
	// gated tool would be auto-denied on every call outside allow-all, so don't
	// advertise a tool that can never run.
	if name == tools.BrowserEval && s.noApprover && s.effectiveMode() != permissions.ModeAllowAll {
		return false
	}
	if name == tools.Reasoning {
		// The executive session only: a read-only sub-agent runs fixed-cheap and must
		// not recursively delegate to the strong lane (cost containment, no nesting).
		return !s.readOnly
	}
	if name == tools.Skill {
		// Offered to the executive (chat + plan) when skills were discovered — never to
		// read-only explorer sub-agents.
		return !s.readOnly && len(s.skills) > 0
	}
	if name == tools.Knowledge {
		// memcode's built-in packs — offered to the executive (chat + plan), never to read-only
		// explorers. Always on (embedded catalog), unlike skill's discovery condition.
		return !s.readOnly
	}
	if name == tools.Artifact {
		// Publishing needs a memcode.ai account — the tool is invisible when the user
		// isn't logged in (like skill's discovery condition). Executive only; the
		// handler additionally restricts publish/update/delete to execution (list
		// works in plan mode for research).
		return !s.readOnly && os.Getenv(provider.EnvAPIToken) != ""
	}
	if name == tools.MCPCodeExec {
		// mcp_code_exec is "code execution with MCP" — its bridge surface is MCP-only,
		// so without connected MCP servers it has nothing to orchestrate and is
		// not advertised (the overlap with the standard repo tools made it a pure
		// read proxy; the repo tools live on the direct path only). Executive only.
		return !s.readOnly && s.mcp != nil && len(s.mcp.Tools()) > 0
	}
	if name == tools.EnterPlan {
		// The executive's door INTO plan mode — normal chat only (not while already planning,
		// not to read-only explorers/reviewers).
		return !s.readOnly && !s.planCtl.Planning()
	}
	if name == tools.CancelPlan || name == tools.ExecutePlan {
		// Offered ONLY while planning: cancel_plan abandons; execute_plan approves+executes (the
		// latter only when the USER says to — see its tool description). Never to read-only explorers.
		return !s.readOnly && s.planCtl.Planning()
	}
	if name == tools.Dispatch {
		// Offload a discrete block to a hands-off background sub-agent. Normal chat only —
		// never from a read-only explorer (no nesting) and never from plan mode (plan mode
		// is research-only; dispatching a mutating agent would bypass the plan→approve gate).
		return !s.readOnly && !s.planCtl.Planning()
	}
	if name == tools.Agent {
		// First-class sub-agent (run-and-report-back). Executive normal chat only: not a
		// read-only explorer, not plan mode (research-only — a mutating agent would bypass the
		// plan→approve gate), and NOT from inside a spawned agent (purpose Agent) so it can't
		// recursively fan out agents.
		return !s.readOnly && !s.planCtl.Planning() && s.purpose != llm.Agent
	}
	if name == tools.RecallPlan {
		// Read-only recall of a saved plan. Offered in normal chat AND plan mode (resuming or
		// revising a prior plan is a core plan-mode use), but not to read-only scout sub-agents.
		return !s.readOnly
	}
	if s.readOnly && s.purpose == llm.Review {
		// The tooled plan reviewer is a CLAIM-VERIFIER, not a general explorer: a strict
		// read-only whitelist (file reads + grep + diff), and NO shell — even read-only
		// bash — so it can't run arbitrary commands while auditing the plan.
		return reviewTool(name)
	}
	if s.readOnly { // explorer
		if name == tools.AskUser {
			return false
		}
		return !isMutatingTool(name) || name == tools.Bash
	}
	if s.planCtl.Planning() { // plan mode: research-only + the gated inspect shell
		return !isMutatingTool(name) || name == tools.Bash
	}
	return true // normal mode
}

// readHash / noteFileHash / forgetFileHash track the content hash of each file memcode
// has read or written, for the stale-edit guard. Guarded by s.mu — read-only tools run
// concurrently.
func (s *Session) readHash(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.metrics.readHashes[path]
	return h, ok
}

func (s *Session) noteFileHash(path, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.metrics.readHashes == nil {
		s.metrics.readHashes = map[string]string{}
	}
	s.metrics.readHashes[path] = hash
}

func (s *Session) forgetFileHash(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.metrics.readHashes, path)
}

// markRead / markEdited / markVerified record turn metrics under s.mu. The edit/verify
// writes previously ran UNLOCKED while read-only tools in the same batch incremented
// toolCalls under the lock — a data race (and lastEditSeq could capture a torn seq,
// corrupting the verified-after-edit completion gate). All metric mutation now takes s.mu.
func (s *Session) markRead() {
	s.mu.Lock()
	s.metrics.filesRead++
	s.mu.Unlock()
}

func (s *Session) markEdited() {
	s.mu.Lock()
	s.metrics.didEdit = true
	s.metrics.lastEditSeq = s.metrics.toolCalls
	s.mu.Unlock()
}

func (s *Session) markVerified(ok bool) {
	s.mu.Lock()
	s.metrics.didVerify = true
	if ok {
		s.metrics.lastVerifyOKSeq = s.metrics.toolCalls
	}
	s.mu.Unlock()
}

// isPDFFile detects a PDF by extension or magic bytes (the %PDF- header) —
// either alone is enough (a mislabeled download still opens; a .pdf that isn't
// one falls through to the text path harmlessly when the magic is absent and
// the extension lies... extension wins so the intent is honored).
func isPDFFile(path string, data []byte) bool {
	return strings.EqualFold(filepath.Ext(path), ".pdf") || strings.HasPrefix(string(data[:min(len(data), 8)]), "%PDF-")
}

// extractPDFText extracts a PDF's text layer locally via pdftotext (poppler)
// when it's installed. ok=false — no extractor, extraction failed, or (an
// image-only scan) no meaningful text — sends the caller down the native-attach
// fallback. Local extraction is the cheap default: a deck attached natively
// bills per-page image tokens; its text layer is a fraction of that.
func extractPDFText(abs string) (string, bool) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-layout", abs, "-").Output()
	if err != nil {
		return "", false
	}
	if len(strings.TrimSpace(string(out))) < 100 {
		return "", false // no real text layer (scan/image deck) → attach natively instead
	}
	return string(out), true
}

func (s *Session) readFile(raw json.RawMessage) toolResult {
	var in tools.ReadFileInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(err.Error())
	}
	abs := resolveReadPath(s.root, in.Path) // reads may reach outside the repo (mirrors cat); writes stay scoped
	s.markRead()
	secret := secrets.IsSecretPath(in.Path)
	data, err := os.ReadFile(abs)
	if err != nil {
		s.toolLine(false, "Read", in.Path, "not found", true)
		return errResult(err.Error())
	}
	// Hash the WHOLE file even on a range read (we read it anyway) — the
	// stale-edit guard must compare against the full content, never a slice.
	s.noteFileHash(in.Path, edit.HashBytes(data))

	// A PDF is not text. Default: extract the text LOCALLY (a few K tokens)
	// and fall through to the normal read path — ranges, truncation, and
	// redaction all apply to the extracted text. Native attachment (the model
	// reads the file itself: layout, charts, scans) is opt-in via attach:true,
	// or the automatic fallback when there's no extractor or no text layer —
	// per-page image billing makes it the expensive path, not the default.
	// Coercing the raw bytes to a string fed the model 64KB of mojibake.
	isPDF := isPDFFile(in.Path, data)
	if isPDF && !in.Attach {
		if text, ok := extractPDFText(abs); ok {
			data = []byte(fmt.Sprintf("[text extracted locally from %s — call read_file with attach:true if you need the document itself (layout/charts/images)]\n%s",
				in.Path, text))
			isPDF = false // continue down the normal text path (range/truncate/redact)
		}
	}
	if isPDF {
		if b64 := input.Base64Len(int64(len(data))); b64 > input.MaxPDFB64Bytes {
			s.toolLine(false, "Read", in.Path, "pdf too large", true)
			return errResult(fmt.Sprintf("%s is %dMB — too large to attach (limit ~%dMB). Extract the pages you need first.",
				in.Path, len(data)>>20, input.MaxPDFB64Bytes*3/4>>20))
		}
		s.toolLine(false, "Read", in.Path, fmt.Sprintf("pdf, %dKB attached", len(data)>>10), false)
		return documentResult(fmt.Sprintf("[attached %s as a PDF document — read it from the document block]", in.Path),
			"application/pdf", data)
	}
	content := string(data)
	total := lineCount(content)

	// Optional line range (1-based, inclusive): a re-verification read of a known
	// region costs the lines it needs, not the whole file re-entering context.
	// The body is deliberately UN-numbered — edit_file anchors on exact text, and
	// numbered output poisons old_string anchors — so a single header line names
	// the slice instead. Sliced from the FULL content, so ranges past the 64KB
	// display cap are reachable too.
	ranged := in.StartLine > 0 || in.EndLine > 0
	var header string
	start, end := in.StartLine, in.EndLine
	if ranged {
		if start < 1 {
			start = 1
		}
		if end < start || end > total {
			end = total
		}
		if start > total {
			return errResult(fmt.Sprintf("start_line %d is past the end of %s (%d lines)", in.StartLine, in.Path, total))
		}
		lines := strings.SplitAfter(content, "\n")
		content = strings.Join(lines[start-1:min(end, len(lines))], "")
		header = fmt.Sprintf("[lines %d-%d of %d — %s]\n", start, end, total, in.Path)
	}

	truncated := len(content) > maxFileRead
	if truncated {
		content = content[:maxFileRead] + "\n…(truncated)"
	}
	if secret {
		// The agent sees the keys/structure, never the values.
		content = secrets.RedactSecretFile(content)
	}
	status := countNoun(total, "line", "lines") // full file, even when display is truncated/sliced
	if ranged {
		status += fmt.Sprintf(" (lines %d-%d)", start, end)
	}
	if truncated {
		status += " (truncated)"
	}
	if secret {
		status += ", masked"
	}
	s.toolLine(false, "Read", in.Path, status, false)
	return textResult(header + content)
}

func (s *Session) listDir(input json.RawMessage) toolResult {
	var in tools.ListDirInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	rel := in.Path
	if rel == "" {
		rel = "."
	}
	abs := resolveReadPath(s.root, rel) // ls may reach outside the repo (mirrors `ls`); writes stay scoped
	entries, err := os.ReadDir(abs)
	if err != nil {
		return errResult(err.Error())
	}
	// Skip hidden entries (.git, .memcode, .claude, dotfiles) UNLESS the listed path is
	// itself a hidden dir — i.e. the user explicitly asked for it. memcode shouldn't
	// operate on hidden state by default; it's noise (and includes memcode's own logs).
	base := filepath.Base(filepath.Clean(rel))
	explicitHidden := strings.HasPrefix(base, ".") && base != "." && base != ".."
	var dirs, files []string
	hidden := 0
	for _, e := range entries {
		if !explicitHidden && strings.HasPrefix(e.Name(), ".") {
			hidden++
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, e.Name()+"/")
		} else {
			files = append(files, e.Name())
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	all := append(dirs, files...)
	s.toolLine(false, "List", rel, countNoun(len(all), "entry", "entries"), false)
	if len(all) == 0 {
		if hidden > 0 {
			return textResult(fmt.Sprintf("(only %d hidden entries — pass the path explicitly to see them)", hidden))
		}
		return textResult("(empty directory)")
	}
	out := strings.Join(all, "\n")
	if hidden > 0 {
		out += fmt.Sprintf("\n(+%d hidden — pass the path explicitly to list)", hidden)
	}
	return textResult(truncate(out, maxToolOutput))
}

// globFiles is repo-native recursive file discovery by name pattern — the guardrailed
// alternative to `bash find`. Backed by repofiles.List (tracked + untracked-not-ignored,
// so .git/.memcode/node_modules/build output are already out), then filtered: hidden
// segments excluded unless include_hidden or the pattern itself names a hidden dir.
func (s *Session) globFiles(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.GlobInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return errResult("glob: a pattern is required (e.g. **/*.md)")
	}
	re, err := globToRegexp(in.Pattern)
	if err != nil {
		return errResult("glob: invalid pattern: " + err.Error())
	}
	max := in.MaxResults
	if max <= 0 || max > 1000 {
		max = 200
	}
	allowHidden := in.IncludeHidden || hasHiddenSegment(in.Pattern)
	var out []string
	hidden, capped := 0, false
	for _, f := range repofiles.List(ctx, s.root) {
		if !allowHidden && hasHiddenSegment(f) {
			hidden++
			continue
		}
		if re.MatchString(f) {
			if len(out) >= max {
				capped = true
				break
			}
			out = append(out, f)
		}
	}
	sort.Strings(out)
	globStatus := countNoun(len(out), "file", "files")
	if capped {
		globStatus += " (capped)"
	}
	s.toolLine(false, "Glob", in.Pattern, globStatus, false)
	if len(out) == 0 {
		msg := fmt.Sprintf("no files match %q", in.Pattern)
		if hidden > 0 {
			msg += fmt.Sprintf(" (%d hidden matches omitted — set include_hidden or name the dir)", hidden)
		}
		return textResult(msg)
	}
	res := strings.Join(out, "\n")
	if capped {
		res += fmt.Sprintf("\n…(capped at %d — narrow the pattern or raise max_results)", max)
	}
	if hidden > 0 {
		res += fmt.Sprintf("\n(+%d hidden omitted — set include_hidden)", hidden)
	}
	return textResult(truncate(res, maxToolOutput))
}

// hasHiddenSegment reports whether any path/pattern segment is a dotfile/dotdir
// (".git", ".memcode", ".github", …) — used to keep hidden state out of file
// discovery unless explicitly requested.
func hasHiddenSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." && seg != ".." {
			return true
		}
	}
	return false
}

// globToRegexp compiles a glob (with ** for any depth) to an anchored regexp over
// slash-separated paths: ** → any segments, * → within a segment, ? → one char.
func globToRegexp(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				if i+2 < len(p) && p[i+2] == '/' {
					b.WriteString("(.*/)?") // **/ → zero or more leading segments
					i += 3
				} else {
					b.WriteString(".*") // ** → anything
					i += 2
				}
				continue
			}
			b.WriteString("[^/]*")
			i++
		case '?':
			b.WriteString("[^/]")
			i++
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func (s *Session) ripgrep(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.RipgrepInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	path := "."
	if in.Path != "" {
		if _, err := safeJoin(s.root, in.Path); err != nil {
			return errResult(err.Error())
		}
		path = in.Path
	}
	// Hard timeout so a pathological search can never hang the turn.
	sctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	// Respect .gitignore (memcode's rule everywhere), and always skip node_modules.
	// The query is a REGEX (rg's default). The git-grep/grep fallbacks default to BASIC
	// regex (BRE), where `|` is a LITERAL char, not alternation — so an `a|b|c` query
	// silently matched NOTHING ("0 matches") whenever rg wasn't a real binary on PATH
	// (a shell-function rg is invisible to exec.LookPath). `-E` puts both fallbacks in
	// EXTENDED regex so alternation/grouping behave like rg.
	var cmd *exec.Cmd
	switch {
	case hasExec("rg"):
		// rg honors .gitignore natively; the glob is the explicit node_modules guard.
		cmd = exec.CommandContext(sctx, "rg", "--line-number", "--no-heading", "--color", "never",
			"--glob", "!**/node_modules/**", in.Query, path)
	case inGitRepo(sctx, s.root):
		// git grep honors .gitignore for tracked AND untracked files (--untracked),
		// so the fallback respects ignores instead of scanning the whole tree.
		cmd = exec.CommandContext(sctx, "git", "-C", s.root, "grep", "-nIE", "--no-color", "--untracked",
			"-e", in.Query, "--", path, ":(exclude,glob)**/node_modules/**")
	default:
		// Last resort (no rg, not a git repo): can't read .gitignore, so exclude a
		// hardcoded set of dependency/build/VCS dirs.
		args := []string{"-rnIE"} // -E: extended regex, so `a|b|c` alternation works (see above)
		for _, d := range searchSkipDirs {
			args = append(args, "--exclude-dir="+d)
		}
		cmd = exec.CommandContext(sctx, "grep", append(args, in.Query, path)...)
	}
	cmd.Dir = s.root
	out, _ := cmd.Output() // non-zero exit just means no matches
	if sctx.Err() == context.DeadlineExceeded {
		s.toolLine(false, "Search", in.Query, "timed out", true)
		return errResult("search timed out after 30s — narrow the query or pass a `path` to scope it.")
	}
	matches := lineCount(strings.TrimRight(string(out), "\n")) // one rg/grep line per match
	s.toolLine(false, "Search", in.Query, countNoun(matches, "match", "matches"), false)
	return textResult(truncate(string(out), maxToolOutput))
}

func hasExec(name string) bool { _, err := exec.LookPath(name); return err == nil }

func inGitRepo(ctx context.Context, root string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--is-inside-work-tree").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// searchSkipDirs are dependency/build/VCS dirs excluded only on the last-resort
// grep path (no rg, no git) — where .gitignore can't be consulted.
var searchSkipDirs = []string{
	"node_modules", ".git", "dist", "build", ".next", "out", "vendor",
	".venv", "venv", "__pycache__", ".turbo", "target", ".cache", ".memcode",
}

func (s *Session) gitDiff(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.GitDiffInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("git_diff: malformed input: " + err.Error())
	}
	args := []string{"-C", s.root, "diff"}
	if in.Path != "" {
		if _, err := safeJoin(s.root, in.Path); err != nil {
			return errResult(err.Error())
		}
		args = append(args, "--", in.Path)
	}
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return errResult("git diff failed: " + err.Error())
	}
	if len(out) == 0 {
		return textResult("(no changes)")
	}
	return textResult(truncate(string(out), maxToolOutput))
}

func (s *Session) editFile(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.EditFileInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	if _, err := safeJoin(s.root, in.Path); err != nil {
		return errResult(err.Error())
	}
	// Transport guard: a CREATE (no old_string anchor) with empty new_string is almost always a
	// DROPPED large payload — the model intended a big file but the content didn't transmit (seen
	// on large writes, on both glm AND opus). Reject with a chunk-it nudge instead of writing a
	// 0-byte file the model then thrashes trying to "fix". (A real empty edit anchors on
	// old_string; intentional empty-file creation is vanishingly rare and `bash: touch` covers it.)
	if in.OldString == "" && strings.TrimSpace(in.NewString) == "" {
		return errResult("edit_file arrived with EMPTY content (no old_string, empty new_string) — for a large file this almost always means the content was too big to transmit in one call. Write it in smaller pieces: create the file with the FIRST chunk, then append the rest with follow-up edits.")
	}
	// A normal edit is Medium. But WEAKENING an existing test/spec/snapshot/fixture
	// (delete/skip a test, drop or change an assertion/expected value) changes WHAT is
	// verified — trust-critical: confirm it even in allow-all, and fail closed when no
	// one can approve. Self-heal must not rewrite the spec to win.
	risk, catastrophic := permissions.Medium, false
	label, detail := "Edit file", "edit_file"
	// Weakening an existing test (delete/skip, or alter an assertion/expected/snapshot/
	// fixture) is a SPEC change. If the user asked to change tests/behavior this turn it's
	// the work (normal policy); otherwise it's a self-heal cheat — confirm even in
	// allow-all, fail closed when headless.
	if isTestPath(in.Path) && weakensTest(in.OldString, in.NewString) && !s.testEditIntent {
		// The THREAT is self-heal silently rewriting a FAILING test to win — gate THAT hard
		// (Dangerous: confirm even in allow-all, fail closed when headless). A deliberate
		// test edit on a NORMAL turn is ordinary, user-visible work (you're iterating on a
		// test, in the loop) — flag it honestly but at NORMAL policy, don't slam a scary
		// "dangerous" gate in front of every test tweak.
		if s.turn.healRounds > 0 {
			risk, catastrophic = permissions.Dangerous, true
			label = "Change what a test verifies"
			detail = "self-heal would weaken an existing test (assertion/expected/snapshot/fixture, or a delete/skip) — confirm only if the old test is stale; otherwise fix the code or ask"
		} else {
			label = "Edit a test assertion" // normal turn → Medium risk (auto-approves in auto mode)
			detail = "changes what an existing test verifies"
		}
	}
	if ok, reason := s.gate(ctx, risk, catastrophic, ApprovalRequest{
		Title: in.Path, Label: label, Detail: detail, Risk: risk.String(),
	}); !ok {
		if catastrophic {
			return errResult("edit blocked: changing what an existing test verifies needs your approval — if tests contradict or one is wrong, ask_user with concrete options instead of editing the test.")
		}
		return errResult("edit denied: " + reason)
	}
	// Stale-edit guard: if the file changed on disk since memcode last read/wrote it
	// (another tool, the user, or CI), the edit is anchored on stale content — refuse and
	// make the agent re-read, rather than clobber or misplace the change.
	if in.OldString != "" {
		if prev, seen := s.readHash(in.Path); seen {
			if cur, ok := edit.Hash(s.root, in.Path); ok && cur != prev {
				s.forgetFileHash(in.Path)
				return errResult(in.Path + " changed on disk since you read it (another tool, the user, or CI) — re-read it and redo the edit against the current content.")
			}
		}
	}
	res, err := edit.Apply(ctx, s.root, in.Path, in.OldString, in.NewString, in.ReplaceAll)
	if err != nil {
		return errResult(err.Error())
	}
	edit.Format(ctx, s.root, res.Path) // auto-format by file type (best-effort, if the tool is installed)
	s.markEdited()
	if s.turn.editedPaths == nil {
		s.turn.editedPaths = map[string]bool{}
	}
	s.turn.editedPaths[res.Path] = true // tracked for the completion gate (see runLoop)
	postHash, _ := edit.Hash(s.root, res.Path)
	s.noteFileHash(res.Path, postHash) // memcode's view of the file is now current
	s.emit(ctx, events.KindFileEdited, map[string]any{"path": res.Path, "created": res.Created, "hash": postHash})
	// Claude-style verbs: Update an existing file, Write a new one.
	verb := "Update"
	if res.Created {
		verb = "Write"
	}
	safeDiff := s.redactor.Redact(res.Diff)
	newContent := s.redactor.Redact(in.NewString)
	if secrets.IsSecretPath(in.Path) {
		// Editing a credential file: mask values in both the diff and new content.
		safeDiff = secrets.RedactSecretFile(safeDiff)
		newContent = secrets.RedactSecretFile(newContent)
	}
	s.toolLine(true, verb, res.Path, "", false)
	if res.Created {
		renderNewFile(s.out, newContent, res.Path, s.diffWidth())
	} else if safeDiff != "" {
		renderDiff(s.out, safeDiff, res.Path, s.diffWidth())
	}
	if safeDiff == "" {
		safeDiff = "(applied; no git diff available)"
	}
	// Self-correction: if the edit left the file unparseable, tell the model so it
	// fixes forward this turn instead of stacking changes on a broken file. The edit
	// stays applied; the warning rides back on the tool result (and is surfaced). We
	// read the FULL post-edit file (in.NewString is only the replaced span).
	out := "OK, " + verb + " " + res.Path + "\n" + truncate(safeDiff, maxToolOutput)
	if abs, err := safeJoin(s.root, res.Path); err == nil {
		if full, err := os.ReadFile(abs); err == nil {
			if warn := validateEdit(res.Path, string(full)); warn != "" {
				s.printf("%s\n", metaStyle.Render("  ⎿  "+strings.SplitN(warn, "\n", 2)[0]))
				out += "\n\n" + warn
			}
		}
	}
	// Post-edit language-server diagnostics (Claude Code style): surface any errors this
	// edit introduced so they're fixed this turn, not discovered at build/runtime.
	if diag := s.editDiagnostics(ctx, res.Path); diag != "" {
		s.printf("%s\n", metaStyle.Render("  ⎿  language server flagged an error in "+res.Path))
		out += diag
	}
	return textResult(out)
}

// applyPatch applies a set of edits ATOMICALLY: validate all, snapshot all target files,
// apply in order, and on ANY failure restore every file (delete files it created) so the
// tree is exactly as it was. This is the coherent-refactor primitive edit_file lacked —
// a rename-plus-callers change lands whole or not at all.
func (s *Session) applyPatch(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.ApplyPatchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	if len(in.Edits) == 0 {
		return errResult("apply_patch needs a non-empty `edits` array.")
	}

	// Risk is the MAX across the edits: a self-heal turn weakening any existing test
	// escalates the whole patch (same doctrine as edit_file), so the batch can't smuggle a
	// spec change through the multi-file path.
	risk, catastrophic := permissions.Medium, false
	for _, e := range in.Edits {
		if isTestPath(e.Path) && weakensTest(e.OldString, e.NewString) && !s.testEditIntent && s.turn.healRounds > 0 {
			risk, catastrophic = permissions.Dangerous, true
		}
	}
	paths := make([]string, len(in.Edits))
	for i, e := range in.Edits {
		paths[i] = e.Path
	}
	if ok, reason := s.gate(ctx, risk, catastrophic, ApprovalRequest{
		Title: fmt.Sprintf("%d files", len(dedupePaths(paths))), Label: "Apply patch (atomic)",
		Detail: strings.Join(dedupePaths(paths), ", "), Risk: risk.String(),
	}); !ok {
		return errResult("apply_patch denied: " + reason)
	}

	// Stale-edit guard for every anchored edit BEFORE touching anything.
	for _, e := range in.Edits {
		if e.OldString == "" {
			continue
		}
		if prev, seen := s.readHash(e.Path); seen {
			if cur, ok := edit.Hash(s.root, e.Path); ok && cur != prev {
				s.forgetFileHash(e.Path)
				return errResult(e.Path + " changed on disk since you read it — re-read it and redo the patch against current content (nothing was applied).")
			}
		}
	}

	// Snapshot each UNIQUE target's current bytes (or mark absent) so we can roll back.
	type snap struct {
		data    []byte
		existed bool
	}
	snaps := map[string]snap{}
	for _, p := range dedupePaths(paths) {
		abs, err := safeJoin(s.root, p)
		if err != nil {
			return errResult("apply_patch: bad path " + p + ": " + err.Error())
		}
		if b, err := os.ReadFile(abs); err == nil {
			snaps[p] = snap{data: b, existed: true}
		} else {
			snaps[p] = snap{existed: false}
		}
	}
	rollback := func() []string {
		var failures []string
		for p, sn := range snaps {
			abs, err := safeJoin(s.root, p)
			if err != nil {
				failures = append(failures, p+": bad path")
				continue
			}
			if sn.existed {
				if err := os.WriteFile(abs, sn.data, 0o644); err != nil {
					failures = append(failures, p+": "+err.Error())
				}
			} else {
				if err := os.Remove(abs); err != nil {
					failures = append(failures, p+": "+err.Error())
				}
			}
		}
		return failures
	}

	// Apply in order; on any failure, roll back EVERYTHING and report.
	var applied []edit.Result
	for i, e := range in.Edits {
		res, err := edit.Apply(ctx, s.root, e.Path, e.OldString, e.NewString, e.ReplaceAll)
		if err != nil {
			rbFailures := rollback()
			msg := fmt.Sprintf("apply_patch: edit %d/%d (%s) failed: %v — rolled back all %d edits, tree unchanged.", i+1, len(in.Edits), e.Path, err, len(in.Edits))
			if len(rbFailures) > 0 {
				msg += fmt.Sprintf(" WARNING: rollback partially failed (%d/%d): %s", len(rbFailures), len(snaps), strings.Join(rbFailures, "; "))
			}
			return errResult(msg)
		}
		applied = append(applied, res)
	}

	// All applied — format, attribute, and note each changed file (same bookkeeping as edit_file).
	if s.turn.editedPaths == nil {
		s.turn.editedPaths = map[string]bool{}
	}
	for _, p := range dedupePaths(paths) {
		edit.Format(ctx, s.root, p)
		s.turn.editedPaths[p] = true
		postHash, _ := edit.Hash(s.root, p)
		s.noteFileHash(p, postHash)
		s.emit(ctx, events.KindFileEdited, map[string]any{"path": p, "hash": postHash, "via": "apply_patch"})
	}
	s.markEdited()

	var b strings.Builder
	fmt.Fprintf(&b, "OK, applied %d edits across %d file(s) atomically:\n", len(applied), len(dedupePaths(paths)))
	for _, res := range applied {
		verb := "Update"
		if res.Created {
			verb = "Write"
		}
		s.toolLine(true, verb, res.Path, "", false)
		if d := s.redactor.Redact(res.Diff); d != "" {
			renderDiff(s.out, d, res.Path, s.diffWidth())
		}
		fmt.Fprintf(&b, "  %s %s\n", verb, res.Path)
	}
	out := strings.TrimRight(b.String(), "\n")
	// Post-edit diagnostics across every changed file (Claude Code style).
	for _, p := range dedupePaths(paths) {
		out += s.editDiagnostics(ctx, p)
	}
	return textResult(out)
}

// dedupePaths returns the unique paths in first-seen order.
func dedupePaths(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func (s *Session) bash(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.BashInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	// Transport guard: an empty command is a dropped payload (a large command that didn't
	// transmit), not a real call — nudge to re-issue / chunk instead of running a blank shell.
	if strings.TrimSpace(in.Command) == "" {
		return errResult("bash arrived with an EMPTY command — the content likely didn't transmit. Re-issue it; if it's a large command, split it into smaller steps.")
	}
	risk, catastrophic := permissions.ClassifyBash(in.Command)
	// In research contexts (plan/explore), resolve the AMBIGUOUS middle (Medium) with
	// a cheap classify-lane check — it can confirm read-only (→ auto-run) or flag a likely
	// mutation (→ confirm). Deterministic Safe/Dangerous are trusted as-is.
	if !catastrophic && (s.planCtl.Planning() || s.readOnly) {
		risk = s.refineRisk(ctx, in.Command, risk)
	}
	// Research contexts are READ-ONLY for bash too. A read-only explorer runs headless (no
	// approver), and plan mode must not mutate — its whole contract is "research now, edits only
	// after you approve and Execute". Without this, the agent can `cat > file` / `db push` through
	// the inspect shell WHILE STILL IN PLAN MODE, half-executing the plan and bypassing the
	// plan→approve→apply state machine (it then re-proposes a plan over already-applied changes).
	// So a may-mutate command is REJECTED here (reported as a step to run on execute), never run.
	if (s.readOnly || s.planCtl.Planning()) && (risk > permissions.Safe || catastrophic) {
		who := "a read-only explorer"
		if s.planCtl.Planning() {
			who = "plan mode (research only — edits happen after you approve and Execute)"
		}
		return errResult("denied: " + who + " runs only read-only commands — this one may modify state. Add it to the PLAN as a step to run on execute; don't run it now.")
	}
	ok, command, reason := s.gateCommand(ctx, risk, catastrophic, in.Command, in.Cwd)
	if !ok {
		return errResult("command denied: " + reason)
	}
	return s.runGatedCommand(ctx, command, in.Cwd, in.Background)
}

// runGatedCommand executes an ALREADY-APPROVED command: resolves cwd, applies the OS
// sandbox, runs it (background job, or foreground with auto-promote), and formats the
// result — everything bash() does after its own gate. Factored out so a caller with its
// OWN single, coarser permission decision (script `run`: one "run this script" approval,
// not a per-command re-classification of what's inside it) can reuse the exact same
// execution/logging/reporting path without going through bash's classify+gate again.
func (s *Session) runGatedCommand(ctx context.Context, command, cwd string, background bool) toolResult {
	dir := s.root
	if cwd != "" {
		// Bash is the explicit, permission-gated escape hatch: an ABSOLUTE cwd is
		// honored anywhere on disk (equivalent to `cd /x && …`, which the classifier
		// sees whole) — it used to be silently joined under root into a fabricated
		// path that failed as "fork/exec /bin/sh: no such file or directory" (the
		// chdir errno blamed on the shell binary). Relative cwd stays repo-scoped.
		d := filepath.Clean(cwd)
		if !filepath.IsAbs(d) {
			var err error
			d, err = safeJoin(s.root, cwd)
			if err != nil {
				return errResult(err.Error())
			}
		}
		// Stat NOW so a bad cwd fails with the real reason the model can act on.
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			return errResult(fmt.Sprintf("cwd %q does not exist", cwd))
		}
		dir = d
	}

	// OS sandbox — defense-in-depth UNDER the classifier gate (see internal/
	// sandbox): read-only shells are always contained; MEMCODE_SANDBOX=1 opts
	// normal sessions into workspace containment. No backend → runs unwrapped.
	// The wrapped line is EXECUTION-ONLY: logs, display, and acceptance keep
	// the user-visible command.
	execCommand := command
	if wrapped, ok := sandbox.Wrap(command, sandbox.PolicyFor(s.readOnly, s.root)); ok {
		execCommand = wrapped
	}

	// Long-running (dev server / watcher): start it as a detached background job and
	// return immediately — NEVER block the turn on something that doesn't exit.
	if background {
		v, err := jobs.Start(s.bgJobs, s.bgCtx, execCommand, dir)
		if err != nil {
			return errResult("failed to start background job: " + err.Error())
		}
		safe := s.redactor.Redact(command)
		slot := s.slotForID(v.ID)
		s.logShell(command+" &", fmt.Sprintf("(shell %d)", slot), 0)
		s.toolLine(true, "Bash", safe, fmt.Sprintf("shell %d", slot), false)
		s.flushAllowNote()
		return textResult(fmt.Sprintf("started shell %d (detached — NOT blocking): %s\nIt keeps running until killed or the session ends. A server doesn't exit, so do NOT wait for it to finish — tell the user where to reach it (the port/URL the command serves) and that they can manage it with /tail %d · /kill %d.", slot, safe, slot, slot))
	}

	// Snapshot the dirty set so we attribute only files THIS command changed
	// (not pre-existing working-tree changes).
	before := changedFiles(ctx, s.root)

	// Foreground with auto-promote: run under the turn ctx (so Esc/Ctrl-C still kills it),
	// but if the command outlives the budget we do NOT kill it — it's promoted to a
	// background shell that keeps running and hands its result back as a NEW turn when it
	// finishes. That's what lets a long build/install ("go install …") run to completion
	// instead of being reaped at the 2-minute deadline.
	oc := jobs.RunForegroundOrPromote(s.bgJobs, s.bgCtx, ctx, bashTimeout, execCommand, dir)
	if oc.Promoted != nil {
		slot := s.slotForID(oc.Promoted.ID)
		safe := s.redactor.Redact(command)
		s.logShell(command, fmt.Sprintf("(shell %d, promoted)", slot), 0)
		s.toolLine(true, "Bash", safe, fmt.Sprintf("running… shell %d", slot), false)
		s.flushAllowNote()
		return textResult(fmt.Sprintf("still running after %s — this command outran the turn budget, so I moved it to background shell %d (detached, NOT killed) and it keeps running. Do NOT wait for it or run anything that depends on its result; do other useful work for now, or wrap up. Its result (exit code + output) will be handed back to you as a NEW turn the moment it finishes. You can watch it with /tail %d or stop it with /kill %d.", bashTimeout, slot, slot, slot))
	}
	exitCode := oc.Exit

	kind := events.KindCommandExecuted
	if looksLikeTest(command) {
		kind = events.KindTestRun
	}
	if looksLikeVerify(command) {
		s.markVerified(exitCode == 0)
	}
	safeCommand := s.redactor.Redact(command)
	s.emit(ctx, kind, map[string]any{"command": safeCommand, "cwd": cwd, "exit": exitCode})

	// Claude-Code-style marker: ⏺ Bash(cmd) with a status-colored bullet, then a short
	// output preview as ONE muted ⎿ block (glyph on the first line, continuations aligned).
	o, e := strings.TrimSpace(s.redactor.Redact(oc.Stdout)), strings.TrimSpace(s.redactor.Redact(oc.Stderr))
	exitReason := "" // "interrupted"/"could not run" instead of a bare "exit -1"
	switch {
	case oc.Killed:
		exitReason = "interrupted"
	case exitCode < 0:
		exitReason = "could not run"
	}
	stat := bashStat(o, exitCode, safeCommand)
	s.toolLineStat(true, "Bash", safeCommand, "", stat)
	s.flushAllowNote()
	preview := bashPreview(o, e, exitCode)
	if exitReason != "" && o == "" && e == "" {
		preview = []string{exitReason} // a killed command with no output: show WHY, not a bare "exit -1"
	}
	s.toolResult(preview)

	// Catch out-of-band edits: files this command newly changed (e.g. via sed).
	for _, f := range newlyChanged(before, changedFiles(ctx, s.root)) {
		s.markEdited()
		postHash, _ := edit.Hash(s.root, f)
		s.noteFileHash(f, postHash) // memcode changed it via bash — keep the stale-guard view current
		s.emit(ctx, events.KindFileEdited, map[string]any{"path": f, "via": "bash", "hash": postHash})
	}

	var b strings.Builder
	if exitReason != "" {
		fmt.Fprintf(&b, "%s\n", exitReason) // interrupted / timed out / could not run — not "exit code: -1"
	} else {
		fmt.Fprintf(&b, "exit code: %d\n", exitCode)
	}
	if o != "" {
		fmt.Fprintf(&b, "--- stdout ---\n%s\n", truncate(o, maxToolOutput))
	}
	if e != "" {
		fmt.Fprintf(&b, "--- stderr ---\n%s\n", truncate(e, maxToolOutput))
	}
	if o == "" && e == "" {
		// Be explicit so the model doesn't mistake a silent success for a broken
		// display: the command ran and printed nothing.
		fmt.Fprintf(&b, "(the command produced no output)\n")
	}
	// Only a real failure is reported as an error to the model. A partial (a chained sequence whose
	// last step returned non-zero but which produced output) is NOT an error — the exit code is in
	// the text above for the model to weigh, but it isn't told the command "failed".
	tr := textResult(b.String())
	tr.isError = stat == statFail
	return tr
}

// bashStat maps a finished bash command to a marker outcome. exit 0 is success. A command that
// couldn't run — negative exit (no exit / signal-killed), 126 (not executable), 127 (not found)
// — is a real failure. A multi-statement SEQUENCE (a; b; c) that produced output but exited
// non-zero is a PARTIAL success: the work ran, only the last step in the chain returned non-zero
// (e.g. a trailing grep that matched nothing). Any other non-zero exit — a single command or
// pipeline that failed (a broken go build/test) — is a real failure.
func bashStat(stdout string, exitCode int, command string) toolStat {
	switch {
	case exitCode == 0:
		return statOK
	case exitCode < 0 || exitCode == 126 || exitCode == 127:
		return statFail
	case permissions.IsSequence(command) && strings.TrimSpace(stdout) != "":
		return statWarn
	default:
		return statFail
	}
}

// runShell executes a user-typed `$` command directly — the direct-shell lane.
// It is NOT an agent turn: no model, no mood/room. The command runs through the
// SAME gate as the bash tool (catastrophic blocked, risky prompts for approval,
// safe/pre-approved run), renders as HIGH-FIDELITY terminal output (the `$` prompt
// plus the command's raw stdout/stderr, byte-for-byte — emitted through the verbatim
// raw path so it never touches the prose/markdown renderer), and is recorded in the
// episodic log so "what did we do earlier?" includes commands you ran by hand.
func (s *Session) runShell(ctx context.Context, command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return
	}
	// Trailing `&` → background job (dev servers, watchers, log tails): start it
	// detached and DON'T block the turn.
	if base := strings.TrimSpace(strings.TrimSuffix(command, "&")); base != command && base != "" {
		s.runShellBackground(ctx, base)
		return
	}
	prompt := shellPromptStyle().Render("$") + " " + command

	risk, catastrophic := permissions.ClassifyBash(command)
	ok, run, reason := s.gateCommand(ctx, risk, catastrophic, command, "")
	// Provenance notes explain why the AGENT's command ran without a prompt. Here the
	// USER typed the command — they ARE the authorization, so a "pre-approved" /
	// "auto-allowed" sub-line is noise. Discard it (don't let it leak onto the next
	// agent surface either).
	s.allowPending = ""
	if !ok {
		s.emitRaw(prompt + "\n" + delStyle().Render("✖ "+orEmpty(reason, "denied")))
		return
	}

	if s.observer != nil {
		s.observer.Busy(true)
		defer s.observer.Busy(false)
	}

	cctx, cancel := context.WithTimeout(ctx, bashTimeout)
	defer cancel()
	before := changedFiles(ctx, s.root)

	cmd := shellCmd(run)
	cmd.Dir = s.root
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := jobs.RunForeground(cctx, cmd) // process-group run: cancel/timeout reaps the whole tree
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	kind := events.KindCommandExecuted
	if looksLikeTest(run) {
		kind = events.KindTestRun
	}
	safeCmd := s.redactor.Redact(run)
	s.emit(ctx, kind, map[string]any{"command": safeCmd, "exit": exitCode, "via": "shell"})
	// Persist command + combined output + exit so it can be recalled as the "last
	// shell result" for explain/fix-last (and shows in recap/commits/search).
	combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	s.logShell(run, combined, exitCode)

	// Build the block VERBATIM: the styled `$` prompt, then the command's raw output
	// exactly as it printed it — no markdown, no branding, blank lines and whitespace
	// preserved. A successful command shows only its output (silent on no output, like
	// a real shell); failures get a red footer.
	var b strings.Builder
	b.WriteString(prompt + "\n")
	if o := strings.TrimRight(sanitizeTermOutput(s.redactor.Redact(stdout.String())), "\n"); o != "" {
		b.WriteString(truncate(o, maxToolOutput) + "\n")
	}
	if e := strings.TrimRight(sanitizeTermOutput(s.redactor.Redact(stderr.String())), "\n"); e != "" {
		b.WriteString(truncate(e, maxToolOutput) + "\n")
	}
	switch exitReason := abnormalExitReason(cctx, runErr, exitCode); {
	case exitReason != "":
		b.WriteString(delStyle().Render("✖ "+exitReason) + "\n") // interrupted / timed out, not "error: context canceled"
	case exitCode != 0:
		b.WriteString(delStyle().Render(fmt.Sprintf("✖ exit %d", exitCode)) + "\n")
	}
	s.emitRaw(strings.TrimRight(b.String(), "\n"))

	// Attribute files this command changed so memcode's state stays accurate.
	for _, f := range newlyChanged(before, changedFiles(ctx, s.root)) {
		s.markEdited()
		postHash, _ := edit.Hash(s.root, f)
		s.noteFileHash(f, postHash) // memcode changed it via $ shell — keep the stale-guard view current
		s.emit(ctx, events.KindFileEdited, map[string]any{"path": f, "via": "shell", "hash": postHash})
	}
}

// sanitizeTermOutput strips terminal CONTROL sequences from captured command output so a
// TUI/interactive app's escapes (alt-screen switch, cursor moves, clears, OSC) can't corrupt
// memcode's own screen when the `$` lane shows the output inline — while KEEPING SGR color
// (ESC[…m) so colored banners and build logs still render, matching how Claude Code surfaces
// captured output. Byte-oriented and UTF-8-safe (multibyte runes are ≥0x80, never confused with
// ESC/C0). Also drops bare \r (overwrite) and other C0 controls except \n and \t.
func sanitizeTermOutput(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b && i+1 < len(s) { // ESC
			switch s[i+1] {
			case '[': // CSI: ESC [ params/intermediates (0x20–0x3f) final (0x40–0x7e)
				j := i + 2
				for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
					j++
				}
				if j < len(s) { // final byte present
					if s[j] == 'm' { // SGR (color/style) — keep verbatim
						b.WriteString(s[i : j+1])
					} // every other CSI (cursor/screen/clear) — drop
					i = j + 1
					continue
				}
				return b.String() // truncated CSI at end — drop the rest
			case ']': // OSC: ESC ] … (BEL | ESC \) — drop entirely (titles, hyperlinks)
				j := i + 2
				for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
					j++
				}
				switch {
				case j >= len(s):
					return b.String()
				case s[j] == 0x07:
					i = j + 1
				default:
					i = j + 2 // ESC \
				}
				continue
			default: // other 2-byte ESC sequence — drop
				i += 2
				continue
			}
		}
		if c < 0x20 && c != '\n' && c != '\t' { // C0 controls (incl. \r) — drop
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// emitRaw prints a block VERBATIM: via the UIObserver's raw path (which the TUI
// routes straight to scrollback, bypassing the markdown/prose renderer) when one is
// attached, else to the plain output writer (already verbatim for the line-REPL).
// High fidelity — no branding, no mangling, whitespace and blank lines intact.
func (s *Session) emitRaw(block string) {
	if s.observer != nil {
		s.observer.Raw(block)
		return
	}
	s.printf("%s\n", block)
}
