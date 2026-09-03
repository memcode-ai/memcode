package runtime

import (
	"context"
	"io"
	"strings"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/jobs"
	"github.com/memcode-ai/memcode/internal/agent/ledger"
	"github.com/memcode-ai/memcode/internal/agent/mood"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/plan"
	"github.com/memcode-ai/memcode/internal/agent/room"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/objectives"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/todos"
	"github.com/memcode-ai/memcode/internal/wire"
)

// UIObserver lets a front-end (the TUI) observe the interactive session's
// routing and lifecycle without scraping stdout. Every method is optional —
// the runtime nil-checks before calling — so a headless caller can ignore it.
// This is a tap on the existing event points in Submit, NOT a change to the
// agent loop itself.
type UIObserver interface {
	// Routed reports how a submitted line was classified.
	Routed(route input.Route, reason string)
	// QueueChanged reports the current queued-task texts (most-recent last).
	QueueChanged(items []string)
	// Busy reports whether a turn is actively running.
	Busy(busy bool)
	// Mood reports the running interaction-friction reading (for the TUI gauge).
	Mood(r mood.Reading)
	// Room reports the assessed room state (mode/intent/policy) for the TUI badge.
	Room(s room.State)
	// Todos reports the agent's current work-tracker checklist (the live region).
	Todos(list todos.List)
	// Tokens reports the running output-token count for the current turn (the ↓
	// counter): a live estimate between the API's usage events, snapped to the
	// authoritative value when it arrives.
	Tokens(output int)
	// Raw prints a block VERBATIM into scrollback — no markdown/prose processing,
	// internal whitespace and blank lines preserved exactly. Used by the `$`
	// direct-shell lane so command output is high-fidelity terminal output, not an
	// assistant-rendered artifact.
	Raw(text string)
}

// SetOutput redirects the session's streamed output to w. The TUI points this
// at a writer that forwards bytes to the Bubble Tea program.
func (s *Session) SetOutput(w io.Writer) { s.out = w }

// SetApprover replaces the approval callback. The TUI routes approvals through
// its own input instead of a separate os.Stdin reader. The callback receives a
// structured ApprovalRequest and returns a structured ApprovalDecision
// (allow / allow-with-edited-command / deny-with-reason / interrupt).
func (s *Session) SetApprover(fn func(context.Context, ApprovalRequest) ApprovalDecision) {
	s.approve = fn
}

// SetAsker replaces the human-in-the-loop question callback. The TUI routes
// ask_user questions through its own selector instead of stdin.
func (s *Session) SetAsker(fn func(context.Context, AskRequest) AskResponse) { s.ask = fn }

// SetObserver attaches a UIObserver (see above). Pass nil to detach.
func (s *Session) SetObserver(o UIObserver) { s.observer = o }

// All cost/token readouts come from the shared Ledger (the metered gateway), so
// they count EVERY model response — main loop, explore sub-agents, overview,
// classify, predict — automatically. The Ledger has its OWN mutex (held only
// briefly during record), independent of s.mu, so reading it from the TUI render
// path is safe: the printf/render deadlock was specifically s.mu held during
// printf, which the Ledger never touches.
//
// The aggregation logic and display types live in internal/agent/ledger; these
// are thin delegation wrappers so callers (vxui /cost, /status) compile unchanged.

// CacheStats returns cumulative prompt-cache token counts (read ≈ 10% cost; write =
// tokens written to the cache). Shown under /debug.
func (s *Session) CacheStats() (read, write int) {
	return ledger.CacheStats(s.runner.Ledger())
}

// Tokens returns cumulative session token flow: total input the model saw (uncached
// + cache reads) and total output. Footer "↑in ↓out".
func (s *Session) Tokens() (in, out int) {
	return ledger.Tokens(s.runner.Ledger())
}

// Spend returns the session's token breakdown and estimated cost in USD (priced per
// response under each call's model; rates are approximate). Powers /cost.
func (s *Session) Spend() (in, out, cacheRead, cacheWrite int, usd float64) {
	return ledger.Spend(s.runner.Ledger())
}

// SpendByBackend returns per-backend usage, busiest first. One entry ("anthropic")
// in a classic session; two once the hybrid router is live.
func (s *Session) SpendByBackend() []ledger.BackendSpend {
	return ledger.SpendByBackend(s.runner.Ledger())
}

// SpendByPurpose returns per-purpose usage, most expensive first — so /cost can show
// where the money actually went (e.g. explore scouts vs the main loop vs synthesis).
func (s *Session) SpendByPurpose() []ledger.PurposeSpend {
	return ledger.SpendByPurpose(s.runner.Ledger())
}

// ContextTokens returns the input size of the latest MAIN conversation call — an
// estimate of how full the context window currently is (footer "ctx N%").
// Read under dispMu (snapshot): the engine writes s.served mid-turn while View reads this
// every frame.
func (s *Session) ContextTokens() int {
	return s.servedSnapshot().ctxTokens
}

// ContextWindow returns the input window (tokens) the meter measures against. It
// prefers the serving backend's REAL window when the gateway reported one (the lane's
// max_model_len — often far smaller than the model-name default), so the meter
// warns before a self-hosted overflow instead of reading a comfortable 14%.
// Falls back to the model's nominal window (Anthropic turns report none).
func (s *Session) ContextWindow() int {
	v := s.servedSnapshot()
	if v.ctxWindow > 0 { // the gateway stamps the served window on every lane now
		return v.ctxWindow
	}
	if s.pin != "" && s.pinWindow > 0 {
		return s.pinWindow // pinned but not yet served: the picker knew the window
	}
	return catalog.ContextWindow(s.model)
}

// SetMode changes the permission mode mid-session (e.g. via a `/mode` command).
func (s *Session) SetMode(m permissions.Mode) {
	s.modeMu.Lock()
	s.mode = m
	s.modeMu.Unlock()
}

// SetPersonality sets the agent's voice (a built-in key or free-text custom voice); empty
// clears it back to the default. Tone only — the gateway guards it from affecting behavior.
// "random" rolls a concrete voice ONCE per session, here — so the ready line can name what
// the roll selected and the session speaks with one voice (a fresh roll each launch keeps
// the chaos; a per-request re-roll made every reply a different character and nothing
// displayable). Re-picking random re-rolls.
func (s *Session) SetPersonality(p string) {
	s.personality = p
	if p == personalityRandom {
		s.personalityRoll = randomPersonality()
	}
}

// PersonalityResolved returns the concrete voice in effect ("" = default): the session's
// random roll when personality is "random", else the chosen voice. Display uses this;
// persistence keeps the literal choice.
func (s *Session) PersonalityResolved() string {
	if s.personality == personalityRandom {
		return s.personalityRoll
	}
	return s.personality
}

// SetExtraMile toggles "extra mile" mode: when on, a fact rides every turn and the gateway
// injects an above-and-beyond rule (edge cases + feature completeness) for planner/executor
// modes. ExtraMile reports the current state.
func (s *Session) SetExtraMile(on bool) { s.extraMile = on }
func (s *Session) ExtraMile() bool      { return s.extraMile }

// SetForceEscalate / SetForceFrontier are DELETED. They pinned a background
// agent's every request to a stronger TIER. Delegated and background workers run
// on the session's pinned model now — the one the user chose and is paying for.

// SetToolPolicy restricts the session to the given toolsets/tools (allow;
// empty = all) minus disabled ones (deny wins) — the gateway applies an
// agent's configured policy here. Unknown entries are reported, not silently
// dropped.
func (s *Session) SetToolPolicy(allow, deny []string) (unknown []string) {
	s.toolPolicy, unknown = tools.NewPolicy(allow, deny)
	return unknown
}

// SetBrowserHeadless makes the lazily-launched Chrome run headless — required
// for gateway/service children with no desktop session.
func (s *Session) SetBrowserHeadless(on bool) { s.browserHeadless = on }

// SetNoApprover marks this session as having no human to answer approval
// prompts (a detached job child). Tools whose every use would be denied (e.g.
// browser_eval at Dangerous outside allow-all) are then not advertised at all
// — a tool that can never run must not be offered.
func (s *Session) SetNoApprover(on bool) { s.noApprover = on }

// SetEffortOverride forces the per-turn thinking effort from the /effort command: "off",
// "medium", or "high" pin it every turn; "auto" (or anything else) clears the override and
// returns to the per-turn heuristic (effortForTurn). EffortOverride reports the current setting.
func (s *Session) SetEffortOverride(level string) {
	switch level {
	case "off":
		s.effortOverride, s.hasEffortOverride = wire.EffortOff, true
	case "medium":
		s.effortOverride, s.hasEffortOverride = wire.EffortMedium, true
	case "high":
		s.effortOverride, s.hasEffortOverride = wire.EffortHigh, true
	default:
		s.hasEffortOverride = false // "auto" → back to the heuristic
	}
}

func (s *Session) EffortOverride() string {
	if !s.hasEffortOverride {
		return "auto"
	}
	switch s.effortOverride {
	case wire.EffortHigh:
		return "high"
	case wire.EffortMedium:
		return "medium"
	default:
		return "off"
	}
}

// Personality returns the chosen voice ("" = default).
func (s *Session) Personality() string { return s.personality }

// SetModel changes the model mid-session (e.g. via a `/model` command).
func (s *Session) SetModel(model string) { s.model = model }

// SetVendor / Vendor are DELETED. They carried a per-session "strong-tier
// vendor" that the Automatic ladder resolved a tier WITHIN. The model names its
// own vendor now, and nothing chooses one on the user's behalf.

// SetPin pins a concrete model for the session (the /model picker's choice): the
// gateway serves this model for every real request; invisible plumbing (classify/
// compact) stays on the utility lanes. label is the gateway's sanitized short name
// ("sonnet", "glm-5p2"); "" = Automatic. window is the pin's context window from
// the picker list (0 = unknown → the SDK catalog sizes the meter). A pin change
// invalidates the learned lane budget/window — the next serve re-teaches them.
//
// A pin change also drops thinking/redacted_thinking blocks from the live chat
// history (if a ChatState is attached via StartChat): those blocks are
// provider-specific — Anthropic validates signatures it issued, and a different
// model (even another Anthropic tier) can't vouch for them. Replaying foreign
// thinking produces "thinking blocks in the latest assistant message cannot be
// modified" (a hard 400). Text and tool blocks are provider-neutral and stay.
func (s *Session) SetPin(label string, window int) {
	s.pin = label
	s.pinWindow = window
	if s.runner != nil {
		s.runner.SetPin(label)
	}
	s.resetServedBudget()
	s.stripThinkingFromLiveChat()
}

// Pin returns the pinned model label ("" = Automatic).
func (s *Session) Pin() string { return s.pin }

// stripThinkingFromLiveChat drops thinking/redacted_thinking blocks from every
// assistant message in the live chat history. Thinking blocks are provider-specific:
// Anthropic validates the signature it issued against the original response, so a
// thinking block from a DIFFERENT model (even another Anthropic tier) replayed on
// the next turn is a "modified" block → hard 400. Text and tool blocks are
// provider-neutral and stay. No-op when no interactive ChatState is attached
// (headless Run/Answer build ephemeral histories and never switch models mid-flight).
func (s *Session) stripThinkingFromLiveChat() {
	st := s.liveChat
	if st == nil {
		return
	}
	changed := false
	for i := range st.messages {
		if st.messages[i].Role != "assistant" {
			continue
		}
		var kept []wire.Block
		for _, b := range st.messages[i].Blocks {
			switch b.Type {
			case "thinking", "redacted_thinking":
				changed = true // drop — provider-specific scratch, not portable across models
			default:
				kept = append(kept, b)
			}
		}
		st.messages[i].Blocks = kept
	}
	_ = changed // no user-facing notice — the switch line already announces the model change
}

// SetDelegatedPin sets the model DELEGATED work runs on: agent-tool workers,
// explore/research scouts, and plan-mode scouts. "" means inherit the primary,
// which is the default — a split only exists because someone asked for one.
//
// This replaced SetScoutModel / SetPlannerModel / SetPlanResearchModel. Those
// set only a sub-session's DISPLAY model and had silently stopped affecting
// which model served anything once the pin became the single selection
// authority — a config knob that looked like it chose a model and didn't.
func (s *Session) SetDelegatedPin(label string, window int) {
	s.delegatedPin, s.delegatedWindow = label, window
}

// DelegatedPin reports the delegated model label; "" when delegated work
// inherits the primary pin.
func (s *Session) DelegatedPin() string { return s.delegatedPin }

// Planning reports whether the session is currently in plan mode.
func (s *Session) Planning() bool { return s.planCtl.Planning() }

// PlanPhaseEpoch returns the plan machine's phase and session epoch atomically —
// the pair the intake gate stamps on a submission so an async relevance verdict
// can be staleness-checked when it lands.
func (s *Session) PlanPhaseEpoch() (plan.Phase, int) { return s.planCtl.PhaseEpoch() }

// PlanPresentable reports whether the most recent plan turn actually rendered a plan to
// approve. The TUI gates the "Plan ready" approval selector on this so an interrupted plan
// turn (Ctrl-C on a clarifying question) never raises a selector with no plan behind it.
func (s *Session) PlanPresentable() bool { return s.planCtl.Presentable() }

// PlanYolo reports whether the current plan was started with /yolo — suppress HITL
// questions during planning and auto-execute the plan without showing the selector.
func (s *Session) PlanYolo() bool { return s.planCtl.Yolo() }

// PlanOpt is a functional option for EnterPlan — an alias for plan.Opt so existing
// callers (WithYolo) compile unchanged.
type PlanOpt = plan.Opt

// WithYolo suppresses human-in-the-loop questions during planning and auto-resolves
// them with the model's recommended choice. The TUI also auto-executes the plan
// without showing the approval selector. Alias for plan.WithYolo.
func WithYolo() PlanOpt { return plan.WithYolo() }

// WithTask anchors this plan session to the task text that started it — the message the
// user typed, or the /plan argument. Alias for plan.WithTask.
func WithTask(task string) PlanOpt { return plan.WithTask(task) }

// applyPlanEffects performs what a plan-machine transition asked for — the ONE
// interpreter of plan.Effects. The machine itself never touches the session,
// the observer, the event log, or the disk (its lock is released before this
// runs, so effects can safely reach printf/observer without the old deadlock).
func (s *Session) applyPlanEffects(ctx context.Context, eff plan.Effects) {
	if eff.ClearTodos && s.observer != nil {
		s.observer.Todos(nil)
	}
	if eff.SetModel != "" {
		s.SetModel(eff.SetModel)
	}
	if eff.SavePlan != "" {
		s.savePlan(eff.SavePlan) // durable ~/.memcode/plans copy; feeds the slug back via RecordSaveSlug
	}
	if eff.Emit != "" {
		s.emit(ctx, eff.Emit, eff.EmitPayload)
	}
}

// EnterPlan switches the session into plan mode: research-only tools, the
// reasoning model, and the planning system prompt. Records plan_started.
// Delegates to the plan.Controller machine; already planning/applying → no-op.
func (s *Session) EnterPlan(ctx context.Context, opts ...PlanOpt) {
	if s.planCtl.InFlow() {
		return // the machine would refuse anyway; bail before wiping session-side state
	}
	// The commit-gate one-shot lives IN the machine now — Enter clears it structurally
	// (the stale-latch reset that used to live here as a remember-to-do line).
	// A fresh plan can't inherit a prior plan's parked-but-undrained deferred messages
	// either. Last-resort wipe only: the TUI's planStart captures leftovers BEFORE calling
	// here and re-parks them against the new plan (so a late classify verdict that missed
	// its plan's drain still runs at the new plan's exit), and the headless chat path
	// drains synchronously after every turn that leaves planning — this nil catches what
	// neither could (panic recovery mid-session), where silently replaying stale text
	// into the NEW plan would be worse.
	s.planDeferred = nil
	s.applyPlanEffects(ctx, s.planCtl.Enter(s.model, opts...))
}

// ExitPlan leaves plan mode, restoring the prior model. approved distinguishes
// an /execute handoff (Approve: pins the contract, arms the apply turn) from a
// /cancel (Cancel: abandon); either way the lifecycle event is recorded.
func (s *Session) ExitPlan(ctx context.Context, approved bool) {
	if !s.planCtl.Planning() {
		return
	}
	task := strings.TrimSpace(s.planCtl.Slug())
	if task == "" {
		task = strings.TrimSpace(s.lastUserText)
	}
	var eff plan.Effects
	if approved {
		eff, _ = s.planCtl.Approve(s.lastText)
	} else {
		eff = s.planCtl.Cancel() // clears the commit-gate one-shot structurally
	}
	s.applyPlanEffects(ctx, eff)
	// A cancelled plan is ABANDONED WORK, not erased work: without an episodic
	// record the focus reducer never learns the arc stopped mid-flight, and the
	// next session's orientation forgets the biggest open thread.
	if !approved && task != "" {
		s.slog.Append(sessionlog.Record{Kind: sessionlog.KindPlanCancelled, Text: s.redactor.Redact(task)})
	}
}

// notePlanTurn records a proposed/revised plan after a planning turn that
// produced output, advancing the revision counter for the memory trail.
func (s *Session) notePlanTurn(ctx context.Context) {
	s.applyPlanEffects(ctx, s.planCtl.NotePlanTurn(strings.TrimSpace(s.lastText) != ""))
}

// SessionID returns the current session id (set by Run/StartChat).
func (s *Session) SessionID() string { return s.sessionID }

// RunningShells snapshots this session's background shells that are still
// running — the live surface behind the footer "N shells" segment and the
// idle-row indicator. In-memory and mutex-cheap, so the TUI may call it on
// the render path (unlike the agent count, which polls the filesystem).
func (s *Session) RunningShells() []jobs.View {
	var out []jobs.View
	for _, v := range s.bgJobs.List() {
		if v.Status == jobs.Running {
			out = append(out, v)
		}
	}
	return out
}

// Model returns the active model id.
func (s *Session) Model() string { return s.model }

// ServingModel returns the model that ACTUALLY served the last main call — which differs
// from Model() when a turn escalates (e.g. an apply turn runs on Opus while the session
// default is Sonnet). The footer uses this so it reflects reality, not the static default.
func (s *Session) ServingModel() string {
	if m := s.servedSnapshot().model; m != "" {
		return m
	}
	// Before any turn has served, prefer the gateway's everyday (cheap-lane) model if we
	// learned it at startup — so the banner/footer don't claim the bootstrap identity.
	if d := s.servingDefaultSnapshot(); d != "" {
		return d
	}
	return s.model
}

// DisplayModel is what the cockpit (footer/banner//status) shows as THE model:
// the /model pin when one is set — the user picked it, so that's the identity,
// even before the first pinned turn serves (ServingModel would still show the
// pre-pin lane) — else the served/default model. The ⇄ line keeps reporting
// per-turn serving reality (absorbs included).
func (s *Session) DisplayModel() string {
	if s.pin != "" {
		return s.pin
	}
	return s.ServingModel()
}

// ServedByok reports whether the LAST main call served on the user's own
// provider key — the footer's per-turn byok segment. Strictly last-turn state:
// recordServed writes it unconditionally, so a non-BYOK turn clears it.
func (s *Session) ServedByok() bool {
	return s.servedSnapshot().byok
}

// ServedBy returns a compact label for WHO served the last call — the cheap-lane model's
// short name (e.g. "glm-5p1") when the cheap lane served it, else the model short name
// ("sonnet"/"haiku"). Used to tag a scout's Explore marker so you can see, per fan-out,
// whether it hit the cheap lane or fell back to Anthropic. "" if nothing served yet.
func (s *Session) ServedBy() string {
	v := s.servedSnapshot() // one snapshot — don't re-enter ServingModel (a second lock/snapshot)
	if isCheapLane(v.backend) && v.pool != "" {
		return v.pool
	}
	if v.backend == "" {
		return ""
	}
	m := v.model
	if m == "" {
		m = s.model
	}
	return provider.ShortModel(m)
}

// ThinkingEffort returns the current turn's thinking-effort label for the status line
// ("low"/"medium"/"high"/…), or "" when thinking is off (so the spinner shows it only
// while the model is actually thinking).
func (s *Session) ThinkingEffort() string {
	s.dispMu.Lock()
	e := s.turnEffort
	s.dispMu.Unlock()
	if e == wire.EffortOff {
		return ""
	}
	return string(e)
}

// ReasoningDisplay returns the HONEST reasoning-depth tier the SERVING model is actually using
// this turn ("high"/"max"/"medium"/"low"), or "" when the model exposes no thinking — for the
// status line. It is lane-aware (reasoningTier): a hybrid open model (GLM etc.) reasons at HIGH on
// ordinary turns and MAX on hard ones, matching the gateway's reasoning_effort policy; an Anthropic
// adaptive model shows its effort tier; anything else shows nothing. This replaces the old label
// that always read "effort: off" — which LIED on the cheap lane (GLM was actually at high/max).
func (s *Session) ReasoningDisplay() string {
	return reasoningTier(s.ServingModel(), wire.Effort(s.ThinkingEffort()))
}

// reasoningTier is the pure model+effort → tier mapping behind ReasoningDisplay (testable without
// a live Session). It mirrors the gateway: the open hybrid models honor reasoning_effort
// (ordinary→high, hard→max); adaptive models (Anthropic Opus/Sonnet, OpenAI GPT-5.6) take the
// effort verbatim (GPT-5.6 maps high→xhigh via the gateway's mapEffort).
func reasoningTier(model string, eff wire.Effort) string {
	m := strings.ToLower(model)
	switch {
	case modelHybridReasoning(m):
		if eff == wire.EffortHigh {
			return "max"
		}
		return "high"
	case modelAnthropicAdaptive(m), isOpenAIReasoning(m):
		switch eff {
		case wire.EffortHigh:
			return "high"
		case wire.EffortMedium:
			return "medium"
		case wire.EffortLow:
			return "low"
		}
	}
	return ""
}

// modelHybridReasoning reports whether a served model is an open hybrid-reasoning model that honors
// the cheap lane's reasoning_effort knob (kept in sync with the gateway's supportsReasoningEffort).
func modelHybridReasoning(model string) bool {
	for _, s := range []string{"glm", "qwen", "deepseek", "kimi", "minimax", "gpt-oss"} {
		if strings.Contains(model, s) {
			return true
		}
	}
	return false
}

// isOpenAIReasoning reports whether a served model is an OpenAI GPT-5.6 model that takes the
// Responses API reasoning.effort knob (kept in sync with the gateway's OpenAI provider).
func isOpenAIReasoning(model string) bool {
	return strings.Contains(strings.ToLower(model), "gpt-5.6")
}

// modelAnthropicAdaptive reports whether a served model takes adaptive thinking + effort (Opus/
// Sonnet) — kept in sync with the gateway's supportsAdaptiveThinking.
func modelAnthropicAdaptive(model string) bool {
	return strings.Contains(model, "opus") || strings.Contains(model, "sonnet")
}

// Mode returns the active permission mode.
func (s *Session) Mode() permissions.Mode {
	s.modeMu.RLock()
	defer s.modeMu.RUnlock()
	return s.mode
}

// Root returns the project root.
func (s *Session) Root() string { return s.root }

// AddObjective records a human-authored goal for this project (the `/goal`
// command). Objectives are never inferred; this is the user asserting one.
func (s *Session) AddObjective(ctx context.Context, title string) (string, error) {
	o, err := objectives.New(s.store).Add(ctx, title, 0, "")
	if err != nil {
		return "", err
	}
	return o.ID, nil
}
