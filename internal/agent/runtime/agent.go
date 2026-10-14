package runtime

import (
	"context"
	"io"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/mood"
	"github.com/memcode-ai/memcode/internal/agent/room"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/policy"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/todos"
	"github.com/memcode-ai/memcode/internal/wire"
)

// agent.go is the ONE sub-agent engine. Every "spawn a sub-agent" path in memcode routes through
// spawnAgent: the read-only research scouts (explore), the plan-mode research + review sessions,
// and the first-class `agent` tool. Before this they were three bespoke bodies; now they differ
// only by the AgentSpec they pass. The engine is assembled from the existing primitives —
// New(...) + runner.Fork() (shared ledger, auto spend attribution), the readOnly tool gate, the
// iterCap bound.
//
// AgentTier (Fast | Strong) is DELETED. It selected which model LANE a sub-agent
// ran on, via a routing hint that escalated it to a stronger tier. A delegated
// worker is user-work inference: it runs on the session's pinned model, the same
// one the user is paying for and watching in the footer.

// AgentSpec is everything that configures a sub-agent. Defaults (zero value) are a
// mutating, MainLoop-ish agent; callers set what they need.
type AgentSpec struct {
	Task     string      // the self-contained instruction the sub-agent runs to completion
	ReadOnly bool        // read-only (no edits/mutating bash) vs a full mutating agent
	Scope    string      // optional subsystem/path tag (telemetry + scout focus)
	IterCap  int         // 0 = mode default
	Purpose  llm.Purpose // ledger attribution; also picks the run shape (Explore → scout prompt)
	Effort   wire.Effort // pin the sub-agent's thinking effort ("" = its own per-turn heuristic)
	// Root roots the sub-agent at another folder ("" = the parent's root). Its
	// file tools are confined there, exactly as the parent's are to its own
	// root: a website worker edits the site's managed copy and nothing else.
	Root string
	// Show streams the worker's activity (its tool lines, edits and notes)
	// into the conversation, indented under the caller's marker, instead of
	// discarding it: for long, user-visible work like building a website.
	Show bool
	// Store is the worker's own state store (nil = the parent's). A worker
	// rooted at another project must not be oriented by this one's memory.
	Store store.Store
	// Site makes the worker a website worker: the site tool, a headless
	// browser of its own, and package managers reachable only through it.
	Site *siteWork
}

// AgentResult is a sub-agent's report-back: its final text plus the telemetry callers surface
// (the tool-count + served-by on the marker line).
type AgentResult struct {
	Text      string
	ToolCalls int
	ServedBy  string // which model actually ran it
}

// spawnAgent runs a sub-agent synchronously and returns its report-back. It is the in-process
// Task primitive: the caller (the LLM via a tool, or plan mode) blocks until the sub-agent
// finishes, then acts on the result.
func (s *Session) spawnAgent(ctx context.Context, spec AgentSpec) (AgentResult, error) {
	// Which model a delegated worker runs on is POLICY, resolved at this
	// decision point. Read-only explorers resolve agent.explore, which declares
	// agent.delegated as its parent, which ends at the session's own model — so
	// the default is still "everything runs on the model you chose", and a
	// split exists only because someone asked for one.
	//
	// Nothing here inspects the task. The target is chosen by the worker's
	// PURPOSE, which the caller already fixed, and the agent tool has no model
	// parameter to override it with.
	target := policy.AgentDelegated
	if spec.Purpose == llm.Explore {
		target = policy.AgentExplore
	}
	model := s.policy.Resolve(target).Model("model")
	runner := s.runner.Fork()
	if model != "" && model != s.pin {
		runner = s.runner.ForkWithModel(model)
	}
	if model == "" {
		model = s.model
	}

	root := s.root
	if spec.Root != "" {
		root = spec.Root
	}
	st := s.store
	if spec.Store != nil {
		st = spec.Store
	}
	var out io.Writer = io.Discard
	if spec.Show {
		out = &nestedOutput{parent: s}
	}
	sub := New(st, runner, root, model, s.effectiveMode(), out)
	if spec.Show {
		if s.width > 8 {
			sub.width = s.width - nestedIndent // its diffs fit beside the rail
		}
		if s.observer != nil {
			sub.observer = toolEvents{s.observer} // protocol clients see the worker's tools too
		}
	}
	sub.policy, sub.pin = s.policy, s.pin // a worker's own workers resolve the same way
	// The run's capability ceiling binds its workers too.
	sub.denyCommands = append([]string(nil), s.denyCommands...)
	if spec.Site != nil {
		sub.site = spec.Site
		sub.browserHeadless = true // its own invisible Chrome, never the user's window
		sub.denyCommands = append(sub.denyCommands, siteWorkerDeny...)
		defer sub.CloseBrowser()
	}
	if spec.Purpose != "" {
		sub.purpose = spec.Purpose
	} else {
		sub.purpose = llm.Agent
	}
	if spec.IterCap > 0 {
		sub.iterCap = spec.IterCap
	}
	if spec.Effort != "" { // a reasoning delegate pins its thinking depth for the whole run
		sub.effortOverride, sub.hasEffortOverride = spec.Effort, true
	}
	// Share the parent's HITL channel so a MUTATING sub-agent's approvals surface in the same
	// TUI cards (the parent turn is blocked here, so there's no contention). Read-only agents
	// never prompt, so this is harmless for them.
	sub.approve, sub.ask = s.approve, s.ask

	// Explore is the read-only RESEARCH shape: scout prompt, evidence-style answer. Answer sets
	// readOnly itself. Everything else is a general task agent on the normal exec prompt, read-only
	// or mutating per the spec, so it can GENERATE/REASON (strong tier) or actually do the work.
	if spec.Purpose == llm.Explore {
		ans, err := sub.Answer(ctx, spec.Scope, spec.Task)
		if err != nil {
			return AgentResult{}, err
		}
		return AgentResult{Text: ans, ToolCalls: sub.metrics.toolCalls, ServedBy: sub.ServedBy()}, nil
	}
	sub.readOnly = spec.ReadOnly
	sub.scope = spec.Scope
	if _, err := sub.Run(ctx, spec.Task); err != nil {
		return AgentResult{}, err
	}
	return AgentResult{Text: sub.lastText, ToolCalls: sub.metrics.toolCalls, ServedBy: sub.ServedBy()}, nil
}

// nestedOutput shows a worker's rendered output inside the parent's, each
// line indented under a dim rail so it reads as the worker's, not the
// conversation's. Partial writes are held until their line completes; runs
// of blank lines collapse to one, and the routing line is the parent's to
// show, not repeated per worker.
// nestedIndent is the rail's width ("  │ ").
const nestedIndent = 4

type nestedOutput struct {
	parent *Session
	mu     sync.Mutex
	buf    strings.Builder
	blank  bool // the last line written was blank
	wrote  bool // anything written yet
}

func (w *nestedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	text := w.buf.String()
	i := strings.LastIndexByte(text, '\n')
	if i < 0 {
		return len(p), nil
	}
	w.buf.Reset()
	w.buf.WriteString(text[i+1:])
	for _, line := range strings.Split(text[:i], "\n") {
		if strings.HasPrefix(strings.TrimSpace(ansi.Strip(line)), "⇄") {
			continue
		}
		if strings.TrimSpace(line) == "" {
			if w.blank || !w.wrote {
				continue
			}
			w.blank = true
			w.parent.printf("%s\n", metaStyle.Render("  │"))
			continue
		}
		w.blank, w.wrote = false, true
		w.parent.printf("%s %s\n", metaStyle.Render("  │"), line)
	}
	return len(p), nil
}

// toolEvents forwards only a worker's tool events to the parent's observer:
// its prose is not the assistant's reply, and its turn state isn't the
// session's.
type toolEvents struct{ UIObserver }

func (t toolEvents) Routed(input.Route, string) {}
func (t toolEvents) QueueChanged([]string)      {}
func (t toolEvents) Busy(bool)                  {}
func (t toolEvents) Mood(mood.Reading)          {}
func (t toolEvents) Room(room.State)            {}
func (t toolEvents) Todos(todos.List)           {}
func (t toolEvents) Tokens(int)                 {}
func (t toolEvents) Raw(string)                 {}
func (t toolEvents) AssistantText(string)       {}
