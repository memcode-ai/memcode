package personal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/continuation"
	"github.com/memcode-ai/memcode/internal/browser/broker"
	"github.com/memcode-ai/memcode/internal/jobs"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Executive is one Personal Agent's bounded decision loop. Each RunOnce is a
// single bounded wake: read durable state, run one LLM turn with domain-neutral
// tools, journal consequential actions, then complete, schedule the next wake,
// or suspend for human input. It never holds an open loop.
type Executive struct {
	Store    *Store
	Home     string
	AgentID  string
	Runner   *llm.Runner
	Now      func() time.Time
	MaxSteps int
	// DelegationDepth is this wake's own depth in a delegation chain — 0 for a
	// top-level RunOnce/ResumeSuspended wake. A worker spawned via delegate is
	// itself a plain `memcode run` job, not another Executive, so depth never
	// grows past 1 today; the field exists so ValidateDelegation's depth check
	// means something even before nested Personal-Agent delegation exists.
	DelegationDepth int
}

type RunOutcome struct {
	RunID         string     `json:"run_id"`
	Status        string     `json:"status"`
	Report        string     `json:"report"`
	NextWakeAt    *time.Time `json:"next_wake_at,omitempty"`
	InteractionID string     `json:"interaction_id,omitempty"`
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

var executiveToolDefs = []wire.ToolDef{
	{
		Name:        "subgoal_update",
		Description: "Create or update an intermediate subgoal beneath the objective. Subgoals are agent-generated planning data and never expand authority. Provide id, description, status (pending|active|done|abandoned|blocked), priority, rationale.",
		InputSchema: obj(map[string]any{
			"id":          strProp("stable subgoal id, e.g. sg-1"),
			"description": strProp("what this subgoal achieves"),
			"status":      strProp("pending|active|done|abandoned|blocked"),
			"priority":    map[string]any{"type": "integer", "description": "higher runs first"},
			"rationale":   strProp("why this subgoal exists"),
		}, "id", "description", "status"),
	},
	{
		Name:        "note_fact",
		Description: "Record a structured fact about the environment with evidence. Facts gate later external representation: only confirmed facts may be presented externally.",
		InputSchema: obj(map[string]any{
			"key":         strProp("fact key, e.g. environment.deps.outdated_count"),
			"value":       map[string]any{"type": "string", "description": "JSON-encoded value"},
			"source":      strProp("where this was observed"),
			"confirmed":   map[string]any{"type": "boolean", "description": "true only if directly verified"},
			"sensitivity": strProp("public|private|secret"),
		}, "key", "value", "source"),
	},
	{
		Name:        "read_file",
		Description: "Read a file inside an approved filesystem grant (observe).",
		InputSchema: obj(map[string]any{"path": strProp("absolute path within a granted filesystem root")}, "path"),
	},
	{
		Name:        "write_file",
		Description: "Write a file inside an approved filesystem grant (local_mutation). Journaled.",
		InputSchema: obj(map[string]any{
			"path":    strProp("absolute path within a granted writable root"),
			"content": strProp("file contents"),
		}, "path", "content"),
	},
	{
		Name:        "schedule_wake",
		Description: "Schedule the next bounded wake for this objective (interval like 30m, or an RFC3339 time). The agent never runs continuously; it must schedule its next wake.",
		InputSchema: obj(map[string]any{
			"after":  strProp("Go duration from now, e.g. 30m"),
			"at":     strProp("RFC3339 timestamp"),
			"reason": strProp("why the next wake is needed"),
		}),
	},
	{
		Name:        "ask_user",
		Description: "Ask the human a question and suspend durably until answered. The whole wake pauses; answer resumes it with exact continuation. Use for missing info, approval, or clarification.",
		InputSchema: obj(map[string]any{
			"question": strProp("the question for the human"),
			"context":  strProp("what the human needs to know to answer"),
		}, "question"),
	},
	{
		Name:        "report",
		Description: "End this wake with a status report and mark the run completed. Include what was done and the next planned step.",
		InputSchema: obj(map[string]any{"summary": strProp("concise status report")}, "summary"),
	},
	{
		Name:        "delegate",
		Description: "Delegate a bounded task to a scoped worker — a full memcode agent (browser, MCP, shell, filesystem, skills — whatever toolsets you name) running as a detached job, NOT another executive. Use this whenever the objective needs a real capability outside this executive's own 7 tools (browsing a site, calling an MCP tool, running a shell command, editing code). The worker's toolset/consequences must be a subset of this agent's own approved policy — expanding authority is rejected. This wake ends without the result; call check_delegate on a later wake (schedule_wake first) to collect it.",
		InputSchema: obj(map[string]any{
			"task":                 strProp("the bounded task for the worker, self-contained (the worker has no access to this conversation)"),
			"expected_output":      strProp("what a successful result looks like"),
			"completion_condition": strProp("how the worker knows it's done"),
			"toolsets":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "toolsets/tools the worker may use, e.g. [\"browser\"], [\"mcp:gmail\"] — must be a subset of this agent's approved allowed_tools. \"browser\" defaults to the user's OWN already-running, already-logged-in Chrome (existing sessions: Gmail, LinkedIn, etc.) — use \"browser:ephemeral\" instead only when the task genuinely wants a fresh, logged-out profile."},
			"consequences":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "consequence classes this task may incur, e.g. [\"observe\"], [\"external_effect\"] — must be a subset of this agent's approved consequence classes"},
			"max_seconds":          map[string]any{"type": "integer", "description": "wall-clock budget for the worker"},
		}, "task", "expected_output", "completion_condition"),
	},
	{
		Name:        "check_delegate",
		Description: "Check on a job started by delegate. Returns its status (running, done, failed, stopped) and, once finished, its result text. Call this on the wake after you delegated, not the same wake.",
		InputSchema: obj(map[string]any{"job_id": strProp("the job id returned by delegate")}, "job_id"),
	},
}

func toolNames(defs []wire.ToolDef) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Name
	}
	return out
}

// RunOnce executes a single bounded wake for the agent's primary objective.
// It fails closed: no approved policy, inactive objective, or an expired/revoked
// policy all block consequential work before any LLM call is made.
func (e *Executive) RunOnce(ctx context.Context) (RunOutcome, error) {
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.MaxSteps <= 0 {
		e.MaxSteps = 8
	}
	now := e.now().UTC()

	obj, ok, err := e.Store.GetObjective(ctx, "primary")
	if err != nil || !ok {
		return RunOutcome{}, fmt.Errorf("no primary objective")
	}
	if obj.Status != "active" && obj.Status != "draft" {
		return RunOutcome{Status: "blocked", Report: "objective is " + obj.Status}, nil
	}
	pol, hasPol, err := e.Store.ApprovedPolicy(ctx, "primary")
	if err != nil {
		return RunOutcome{}, err
	}
	if !hasPol {
		return RunOutcome{Status: "blocked", Report: "no approved policy — consequential work is blocked until you run `memcode personal approve-policy`"}, nil
	}
	var policyDoc DelegationPolicy
	if err := json.Unmarshal(pol.Document, &policyDoc); err != nil {
		return RunOutcome{}, fmt.Errorf("approved policy is corrupt: %w", err)
	}
	if !policyDoc.AllowsConsequence(Observe, now) {
		return RunOutcome{Status: "blocked", Report: "approved policy is expired or revoked"}, nil
	}
	if err := ValidateExecutiveBudget(nonzero(policyDoc.MaxSeconds, 600), nonzero(policyDoc.MaxActionsPerPeriod, e.MaxSteps), policyDoc.MaxDelegationDepth); err != nil {
		return RunOutcome{}, err
	}

	// Filter tools to the policy's allowlist (deny wins; empty = all non-suspending core).
	tools := e.allowedTools(policyDoc)
	if len(tools) == 0 {
		return RunOutcome{Status: "blocked", Report: "policy allows no executive tools"}, nil
	}

	runID := fmt.Sprintf("run-%d", now.UnixNano())
	env, _ := json.Marshal(map[string]any{"agent": e.AgentID, "policy_hash": pol.Hash, "tools": toolNames(tools)})
	if err := e.Store.CreateRun(ctx, Run{ID: runID, ObjectiveID: "primary", Status: "running", Envelope: env}); err != nil {
		return RunOutcome{}, err
	}

	// Build the opening user turn; the durable objective/subgoal/fact state rides
	// as the doctrine `state` fact (Mode: personal).
	msgs := []wire.Message{{Role: "user", Blocks: []wire.Block{wire.TextBlock("Advance the objective with one bounded step. Call report when done, schedule_wake to set the next wake, or ask_user if you need the human.")}}}
	out := e.loop(ctx, runID, policyDoc, pol.Hash, msgs, tools)
	_ = e.Store.UpdateRunStatus(ctx, runID, out.Status, json.RawMessage(fmt.Sprintf(`{"report":%q}`, out.Report)))
	return out, nil
}

// loop runs the bounded tool-call loop over a message history. Shared by
// RunOnce and resume: resume re-enters with the saved transcript plus the
// answered tool_result appended.
func (e *Executive) loop(ctx context.Context, runID string, policyDoc DelegationPolicy, policyHash string, msgs []wire.Message, tools []wire.ToolDef) RunOutcome {
	if e.MaxSteps <= 0 {
		e.MaxSteps = 8
	}
	var out RunOutcome
	out.RunID = runID
	out.Status = "completed"
	for step := 0; step < e.MaxSteps; step++ {
		resp, err := e.Runner.Complete(ctx, llm.MainLoop, wire.Request{
			Mode:     "personal",
			Facts:    map[string]string{"state": e.stateSummary(policyDoc)},
			Messages: msgs,
			Tools:    tools,
		})
		if err != nil {
			out.Status = "failed"
			out.Report = "model error: " + err.Error()
			_ = e.Store.UpdateRunStatus(ctx, runID, "failed", json.RawMessage(fmt.Sprintf(`{%q:%q}`, "error", out.Report)))
			return out
		}
		assistant := wire.Message{Role: "assistant", Blocks: resp.Blocks}
		msgs = append(msgs, assistant)

		// Partition tool calls; detect a suspension (must be sole tool use).
		var calls []wire.Block
		var text strings.Builder
		for _, b := range resp.Blocks {
			if b.Type == "tool_use" {
				calls = append(calls, b)
			}
			if b.Type == "text" {
				text.WriteString(b.Text)
			}
		}
		if resp.StopReason != "tool_use" || len(calls) == 0 {
			// Model ended the turn with text.
			out.Report = strings.TrimSpace(text.String())
			break
		}
		// Handle each tool call, collecting results.
		var results []wire.Block
		suspended := false
		reported := false
		for _, c := range calls {
			res, susp, err := e.execTool(ctx, runID, policyDoc, policyHash, c, msgs)
			if err != nil {
				results = append(results, wire.Block{Type: "tool_result", ToolUseID: c.ID, Content: "error: " + err.Error(), IsError: true})
				continue
			}
			if susp != nil {
				// Suspension must be the sole tool use; persist exact continuation.
				if len(calls) != 1 {
					results = append(results, wire.Block{Type: "tool_result", ToolUseID: c.ID, Content: "ask_user must be the only tool call in a response", IsError: true})
					continue
				}
				out.Status = "suspended"
				out.InteractionID = susp.ID
				out.Report = "waiting for human: " + susp.Question
				suspended = true
				break
			}
			if res.report != "" {
				out.Report = res.report
				reported = true
			}
			if res.nextWake != nil {
				out.NextWakeAt = res.nextWake
			}
			results = append(results, wire.Block{Type: "tool_result", ToolUseID: c.ID, Content: res.content})
		}
		if suspended {
			_ = e.Store.UpdateRunStatus(ctx, runID, "waiting", json.RawMessage(fmt.Sprintf(`{"interaction_id":%q}`, out.InteractionID)))
			return out
		}
		// report ends the wake: its summary is the run's report.
		if reported {
			break
		}
		msgs = append(msgs, wire.Message{Role: "user", Blocks: results})
	}
	return out
}

func nonzero(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

func (e *Executive) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// allowedTools filters the executive toolset by policy. The policy's
// AllowedTools is the primary gate: when non-empty, only those tools are
// exposed. Consequence classes are a second gate — a mutation/external tool is
// exposed only if both listed AND its consequence class is allowed. Observe/
// planning tools still require their (implicit) class to pass.
func (e *Executive) allowedTools(p DelegationPolicy) []wire.ToolDef {
	now := e.now().UTC()
	allowed := map[string]bool{}
	restrictByName := len(p.AllowedTools) > 0
	for _, t := range p.AllowedTools {
		allowed[t] = true
	}
	// consequence requirement per tool
	need := map[string]ConsequenceClass{
		"read_file":  Observe,
		"write_file": LocalMutation,
	}
	var out []wire.ToolDef
	for _, d := range executiveToolDefs {
		if restrictByName && !allowed[d.Name] {
			continue // not in the policy's allowlist
		}
		if cons, ok := need[d.Name]; ok && !p.AllowsConsequence(cons, now) {
			continue // consequence class not granted
		}
		out = append(out, d)
	}
	return out
}

// stateSummary renders the durable objective/subgoal/fact state as the doctrine
// `state` fact for the personal mode. It is data, not prompt prose.
func (e *Executive) stateSummary(p DelegationPolicy) string {
	o, ok, err := e.Store.GetObjective(context.Background(), "primary")
	if err != nil || !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Objective: %s\n", o.Description)
	if o.SuccessCriteria != "" {
		fmt.Fprintf(&b, "Success criteria: %s\n", o.SuccessCriteria)
	}
	fmt.Fprintf(&b, "Policy consequence classes: %v; delegation depth %d.\n", p.ConsequenceClasses, p.MaxDelegationDepth)
	if subs, err := e.Store.ListSubgoals(context.Background(), o.ID); err == nil && len(subs) > 0 {
		b.WriteString("Current subgoals:\n")
		for _, g := range subs {
			fmt.Fprintf(&b, "  - [%s] %s (%s)\n", g.Status, g.Description, g.ID)
		}
	}
	if facts, err := e.Store.ListFacts(context.Background(), o.ID); err == nil && len(facts) > 0 {
		b.WriteString("Known facts:\n")
		for _, f := range facts {
			fmt.Fprintf(&b, "  - %s = %s (source %s)\n", f.Key, string(f.Value), f.Source)
		}
	}
	return b.String()
}

type toolResult struct {
	content  string
	report   string
	nextWake *time.Time
}

type suspensionInfo struct {
	ID, Question string
}

// execTool runs one executive tool under the policy. It returns a result, or a
// suspension if the tool is ask_user.
func (e *Executive) execTool(ctx context.Context, runID string, p DelegationPolicy, policyHash string, call wire.Block, msgs []wire.Message) (toolResult, *suspensionInfo, error) {
	now := e.now().UTC()
	journaling := func(kind, target string, cons ConsequenceClass, req json.RawMessage) (string, error) {
		actID := fmt.Sprintf("act-%d", now.UnixNano())
		_, fresh, err := e.Store.ReserveAction(ctx, ActionIntent{
			ID: actID, ObjectiveID: "primary", RunID: runID, Kind: kind, Target: target,
			Consequence: cons, PolicyHash: policyHash, Request: req,
		})
		if err != nil {
			return "", err
		}
		if !fresh {
			return "", fmt.Errorf("duplicate action rejected")
		}
		return actID, e.Store.MarkActionRunning(ctx, actID)
	}

	switch call.Name {
	case "subgoal_update":
		var in struct {
			ID, Description, Status, Rationale string
			Priority                           int
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return toolResult{}, nil, err
		}
		err := e.Store.UpsertSubgoal(ctx, Subgoal{ID: in.ID, ObjectiveID: "primary", Description: in.Description, Status: in.Status, Priority: in.Priority, Rationale: in.Rationale})
		if err != nil {
			return toolResult{}, nil, err
		}
		return toolResult{content: "subgoal " + in.ID + " recorded"}, nil, nil

	case "note_fact":
		var in struct {
			Key, Value, Source, Sensitivity string
			Confirmed                       bool
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return toolResult{}, nil, err
		}
		f := Fact{ID: fmt.Sprintf("fact-%d", now.UnixNano()), ObjectiveID: "primary", Key: in.Key,
			Value: json.RawMessage(in.Value), Source: in.Source, Confirmed: in.Confirmed, Sensitivity: in.Sensitivity}
		if err := e.Store.InsertFact(ctx, f); err != nil {
			return toolResult{}, nil, err
		}
		return toolResult{content: "fact recorded: " + in.Key}, nil, nil

	case "read_file":
		var in struct{ Path string }
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return toolResult{}, nil, err
		}
		data, err := e.readGranted(in.Path)
		if err != nil {
			return toolResult{}, nil, err
		}
		return toolResult{content: data}, nil, nil

	case "write_file":
		var in struct{ Path, Content string }
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return toolResult{}, nil, err
		}
		if !p.AllowsConsequence(LocalMutation, now) {
			return toolResult{}, nil, fmt.Errorf("policy does not allow local_mutation")
		}
		actID, err := journaling("write_file", in.Path, LocalMutation, call.Input)
		if err != nil {
			return toolResult{}, nil, err
		}
		if err := e.writeGranted(in.Path, in.Content); err != nil {
			_ = e.Store.CompleteAction(ctx, actID, ActionFailed, json.RawMessage(fmt.Sprintf(`{%q:%q}`, "error", err.Error())), nil)
			return toolResult{}, nil, err
		}
		_ = e.Store.CompleteAction(ctx, actID, ActionSucceeded, nil, nil)
		return toolResult{content: "wrote " + in.Path}, nil, nil

	case "schedule_wake":
		var in struct{ After, At, Reason string }
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return toolResult{}, nil, err
		}
		var next time.Time
		if in.After != "" {
			d, err := time.ParseDuration(in.After)
			if err != nil {
				return toolResult{}, nil, err
			}
			next = now.Add(d)
		} else if in.At != "" {
			t, err := time.Parse(time.RFC3339, in.At)
			if err != nil {
				return toolResult{}, nil, err
			}
			next = t
		} else {
			return toolResult{}, nil, fmt.Errorf("schedule_wake needs after or at")
		}
		tid := fmt.Sprintf("wake-%d", next.Unix())
		if err := e.Store.CreateTrigger(ctx, Trigger{ID: tid, ObjectiveID: "primary", Kind: "next_wake", Spec: next.Format(time.RFC3339), NextDueAt: &next}); err != nil {
			return toolResult{}, nil, err
		}
		return toolResult{content: "next wake at " + next.Format(time.RFC3339), nextWake: &next}, nil, nil

	case "delegate":
		var in struct {
			Task                string   `json:"task"`
			ExpectedOutput      string   `json:"expected_output"`
			CompletionCondition string   `json:"completion_condition"`
			Toolsets            []string `json:"toolsets"`
			Consequences        []string `json:"consequences"`
			MaxSeconds          int      `json:"max_seconds"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return toolResult{}, nil, err
		}
		var consequences []ConsequenceClass
		for _, c := range in.Consequences {
			cls := ConsequenceClass(c)
			if !p.AllowsConsequence(cls, now) {
				return toolResult{}, nil, fmt.Errorf("policy does not allow delegating consequence %q", c)
			}
			consequences = append(consequences, cls)
		}
		browserSession := browserModeFor(in.Toolsets)
		env := ExecutionEnvelope{
			Task: in.Task, ExpectedOutput: in.ExpectedOutput, CompletionCondition: in.CompletionCondition,
			Toolsets: in.Toolsets, Consequences: consequences, ParentRunID: runID,
			Budgets:         jobs.ExecutionBudgets{MaxSeconds: in.MaxSeconds},
			DelegationDepth: e.DelegationDepth + 1,
			AllowDelegation: false, // the worker is a plain memcode run, not another executive — it cannot delegate further
			BrowserSession:  browserSession,
		}
		if err := ValidateDelegation(p, env); err != nil {
			return toolResult{}, nil, err
		}
		if browserSession == BrowserExistingChrome {
			// Fail closed BEFORE spawning anything: a worker that can't reach
			// the broker must never silently run with ephemeral (logged-out)
			// Chrome instead — that would complete "successfully" while doing
			// something other than what was asked and authorized.
			sock, err := broker.SocketPath()
			if err != nil || !broker.NewClient(sock).Reachable() {
				return toolResult{}, nil, fmt.Errorf("existing-Chrome is not available (gateway not running, or `memcode personal browser setup` not completed) — refusing to fall back to ephemeral Chrome")
			}
		}
		actID, err := journaling("delegate", in.Task, delegateConsequence(consequences), call.Input)
		if err != nil {
			return toolResult{}, nil, err
		}
		workDir, err := e.delegateRoot()
		if err != nil {
			_ = e.Store.CompleteAction(ctx, actID, ActionFailed, json.RawMessage(fmt.Sprintf(`{%q:%q}`, "error", err.Error())), nil)
			return toolResult{}, nil, err
		}
		if _, err := PrepareRunDirectory(e.Home, runID, env); err != nil {
			_ = e.Store.CompleteAction(ctx, actID, ActionFailed, json.RawMessage(fmt.Sprintf(`{%q:%q}`, "error", err.Error())), nil)
			return toolResult{}, nil, err
		}
		job, err := jobs.SpawnWithSpec(jobs.SpawnSpec{
			Root:       workDir,
			Task:       fmt.Sprintf("%s\n\nExpected output: %s\nDone when: %s", in.Task, in.ExpectedOutput, in.CompletionCondition),
			Mode:       delegateMode(consequences),
			ToolPolicy: jobs.ToolPolicy{Allowed: in.Toolsets},
			Budgets:    env.Budgets,
			AgentID:    e.AgentID, ObjectiveID: "primary", RunID: runID, ParentRunID: runID,
			PolicyHash: policyHash, BrowserMode: browserSession, ReportBack: true,
		})
		if err != nil {
			_ = e.Store.CompleteAction(ctx, actID, ActionFailed, json.RawMessage(fmt.Sprintf(`{%q:%q}`, "error", err.Error())), nil)
			return toolResult{}, nil, err
		}
		// Record the job↔action mapping as a fact so check_delegate can find the
		// action to complete later; RunOnce is one bounded wake, so the result
		// necessarily arrives on a subsequent wake, not this one.
		mapping, _ := json.Marshal(map[string]any{"action_id": actID, "task": in.Task, "status": "running"})
		_ = e.Store.InsertFact(ctx, Fact{ID: fmt.Sprintf("fact-%d", now.UnixNano()), ObjectiveID: "primary",
			Key: "delegation." + job.ID, Value: mapping, Source: "delegate", Confirmed: true})
		return toolResult{content: fmt.Sprintf("delegated as job %s — call check_delegate on a later wake to collect the result", job.ID)}, nil, nil

	case "check_delegate":
		var in struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return toolResult{}, nil, err
		}
		workDir, err := e.delegateRoot()
		if err != nil {
			return toolResult{}, nil, err
		}
		job, err := jobs.Get(workDir, in.JobID)
		if err != nil {
			return toolResult{}, nil, fmt.Errorf("no delegated job %q: %w", in.JobID, err)
		}
		if job.Status == jobs.StatusRunning || job.Status == jobs.StatusWaiting {
			return toolResult{content: fmt.Sprintf("job %s still %s", job.ID, job.Status)}, nil, nil
		}
		actID := e.delegationActionID(ctx, in.JobID)
		status, result := ActionSucceeded, json.RawMessage(fmt.Sprintf(`{"result":%q}`, job.Result))
		if job.Status != jobs.StatusDone || job.ExitCode != 0 {
			status, result = ActionFailed, json.RawMessage(fmt.Sprintf(`{"status":%q,"exit_code":%d}`, job.Status, job.ExitCode))
		}
		if actID != "" {
			_ = e.Store.CompleteAction(ctx, actID, status, result, nil)
		}
		return toolResult{content: fmt.Sprintf("job %s %s: %s", job.ID, job.Status, job.Result)}, nil, nil

	case "ask_user":
		var in struct{ Question, Context string }
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return toolResult{}, nil, err
		}
		interactionID := fmt.Sprintf("int-%d", now.UnixNano())
		// Persist the durable interaction (fails if DB write fails → no suspension).
		if err := e.Store.InsertInteraction(ctx, Interaction{
			ID: interactionID, AgentID: e.AgentID, ObjectiveID: "primary", RunID: runID,
			Kind: "question", Question: in.Question, Context: in.Context, ToolUseID: call.ID, Status: "pending",
		}); err != nil {
			return toolResult{}, nil, fmt.Errorf("could not persist interaction: %w", err)
		}
		// Persist the exact continuation for resume (transcript + tool_use_id).
		assistant := msgs[len(msgs)-1]
		if err := writeSuspension(e.Home, runID, interactionID, call, assistant, msgs); err != nil {
			return toolResult{}, nil, fmt.Errorf("could not persist continuation: %w", err)
		}
		return toolResult{}, &suspensionInfo{ID: interactionID, Question: in.Question}, nil

	case "report":
		var in struct{ Summary string }
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return toolResult{}, nil, err
		}
		return toolResult{content: "reported", report: in.Summary}, nil, nil

	default:
		return toolResult{}, nil, fmt.Errorf("unknown executive tool %q", call.Name)
	}
}

// delegateRoot is the project root a delegated worker runs in: the agent's own
// workspace (the same directory write_file treats as always-writable). A
// worker that needs a different project is future work — for now every
// delegated job is rooted here, and jobs.Get must be called against the same
// root to find it again.
func (e *Executive) delegateRoot() (string, error) {
	dir := filepath.Join(e.Home, "workspace")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// delegationActionID recovers the action id a delegate call recorded for jobID
// (as a fact, since facts are the only durable log delegate can write to
// without a schema migration), so check_delegate can close it out.
func (e *Executive) delegationActionID(ctx context.Context, jobID string) string {
	facts, err := e.Store.ListFacts(ctx, "primary")
	if err != nil {
		return ""
	}
	key := "delegation." + jobID
	for i := len(facts) - 1; i >= 0; i-- {
		if facts[i].Key != key {
			continue
		}
		var v struct {
			ActionID string `json:"action_id"`
		}
		if json.Unmarshal(facts[i].Value, &v) == nil {
			return v.ActionID
		}
	}
	return ""
}

// delegateConsequence reports the highest-stakes consequence class in a
// delegated task, for the action journal entry (ReserveAction needs exactly
// one). Order matches the severity ExecutionEnvelope.Consequences is checked
// in: an empty request journals as pure observation.
func delegateConsequence(cs []ConsequenceClass) ConsequenceClass {
	order := []ConsequenceClass{Destructive, LegalAttestation, Financial, ExternalRepresentation, ExternalEffect, LocalMutation, Observe}
	have := map[ConsequenceClass]bool{}
	for _, c := range cs {
		have[c] = true
	}
	for _, c := range order {
		if have[c] {
			return c
		}
	}
	return Observe
}

// delegateMode picks the worker's permission mode from what it's authorized to
// do. Detached jobs have no human to answer approval prompts (SetNoApprover),
// so --ask is the fail-closed choice for anything beyond safe local mutation:
// the worker simply can't perform an action requiring approval, rather than
// silently getting more authority than the policy actually granted it.
func delegateMode(cs []ConsequenceClass) string {
	for _, c := range cs {
		if c != Observe && c != LocalMutation {
			return "ask"
		}
	}
	return "auto"
}

// browserModeFor reports the jobs.SpawnSpec.BrowserMode for a requested
// toolset list. A bare "browser" defaults to BrowserExistingChrome — the
// user's own already-running, already-logged-in Chrome, reached through the
// gateway-owned broker (see internal/browser/broker) — because a Personal
// Agent's whole point is acting as the user, and most useful browser work
// (Gmail, LinkedIn, an ATS, an internal dashboard) requires being signed in.
// "browser:ephemeral" is the explicit opt-down to a fresh, logged-out
// profile, for tasks that genuinely don't want the user's session (e.g.
// visiting a site anonymously). See docs/design/personal-agents.md "Browser
// broker trust boundary".
func browserModeFor(toolsets []string) string {
	for _, t := range toolsets {
		switch t {
		case "browser", "browser:existing_chrome":
			return BrowserExistingChrome
		case "browser:ephemeral":
			return BrowserEphemeral
		}
	}
	return ""
}

// readGranted reads a file only if it lies within an approved filesystem grant.
func (e *Executive) readGranted(path string) (string, error) {
	res, err := e.Store.ListResources(context.Background(), "primary")
	if err != nil {
		return "", err
	}
	// The agent's own home is always readable.
	if PathWithinGrant(path, e.Home) {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	for _, r := range res {
		if r.Type == "filesystem" && r.Status == "active" && PathWithinGrant(path, r.Locator) {
			b, err := os.ReadFile(path)
			return string(b), err
		}
	}
	return "", fmt.Errorf("path %s is not within an approved filesystem grant", path)
}

func (e *Executive) writeGranted(path, content string) error {
	res, err := e.Store.ListResources(context.Background(), "primary")
	if err != nil {
		return err
	}
	writable := func(r Resource) bool {
		return r.Type == "filesystem" && r.Status == "active" && (r.AccessMode == "write" || r.AccessMode == "admin") && PathWithinGrant(path, r.Locator)
	}
	// The generated workspace is always writable (agent-owned).
	if PathWithinGrant(path, filepath.Join(e.Home, "workspace")) {
		return os.WriteFile(path, []byte(content), 0o600)
	}
	for _, r := range res {
		if writable(r) {
			return os.WriteFile(path, []byte(content), 0o600)
		}
	}
	return fmt.Errorf("path %s is not within a writable approved filesystem grant", path)
}

// suspensionDir is where this run's continuations live. The executive keeps no
// transcript between wakes, so its continuations sit beside the run rather than
// under a session directory (see continuation.SessionDir for the interactive
// layout).
func suspensionDir(home, runID string) string {
	return filepath.Join(home, "runs", runID)
}

// writeSuspension persists the exact continuation for an ask_user suspension.
// It stores the full message transcript (Messages) because a wake rebuilds its
// context from durable state and has no transcript to append to on resume.
func writeSuspension(home, runID, interactionID string, call wire.Block, assistant wire.Message, msgs []wire.Message) error {
	return continuation.Save(suspensionDir(home, runID), continuation.Suspension{
		RunID: runID, InteractionID: interactionID,
		ToolUseID: call.ID, ToolName: call.Name, ToolInput: json.RawMessage(call.Input),
		Assistant: assistant, Messages: msgs,
	})
}

// ResumeSuspended continues a suspended run after its interaction is answered.
// It loads the saved transcript, appends the exact tool_result for the suspended
// tool_use_id, then re-enters the bounded loop so the model actually continues —
// no replay of completed actions, no fabricated user turn. It marks the
// continuation resolved ONLY after the resumed run finishes, so a failure leaves
// the interaction retryable.
func (e *Executive) ResumeSuspended(ctx context.Context, in Interaction, answer string) (RunOutcome, error) {
	dir := suspensionDir(e.Home, in.RunID)
	s, err := continuation.Load(dir, in.ID)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("no resumable continuation for interaction %q: %w", in.ID, err)
	}
	// Re-load the approved policy (it may have narrowed since suspension).
	pol, hasPol, err := e.Store.ApprovedPolicy(ctx, "primary")
	if err != nil {
		return RunOutcome{}, err
	}
	if !hasPol {
		return RunOutcome{}, fmt.Errorf("policy was revoked while suspended — cannot resume")
	}
	var policyDoc DelegationPolicy
	if err := json.Unmarshal(pol.Document, &policyDoc); err != nil {
		return RunOutcome{}, err
	}
	tools := e.allowedTools(policyDoc)

	// Rebuild the transcript with the exact tool result matching the suspended
	// tool_use_id. Not marked resolved yet — see below.
	msgs, err := s.ResumeMessages(wire.Block{Type: "tool_result", ToolUseID: s.ToolUseID, Content: answer})
	if err != nil {
		return RunOutcome{}, err
	}

	out := e.loop(ctx, in.RunID, policyDoc, pol.Hash, msgs, tools)

	// Mark the continuation resolved once the answer has been consumed: either the
	// run reached a terminal state, or it re-suspended on a new interaction (whose
	// own continuation carries the appended answer forward). A hard resume error
	// leaves it unresolved on purpose, so the answer can be given again.
	if out.Status == "completed" || out.Status == "failed" || out.Status == "suspended" {
		_ = continuation.MarkResolved(dir, in.ID)
		_ = e.Store.UpdateRunStatus(ctx, in.RunID, out.Status, json.RawMessage(fmt.Sprintf(`{"report":%q}`, out.Report)))
	}
	return out, nil
}
