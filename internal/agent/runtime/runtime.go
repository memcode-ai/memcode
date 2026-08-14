// Package runtime is the agent loop: orient via the context compiler, call
// Claude, execute the tool calls it proposes (under the permission gate),
// capture every step as an event, and stop when the model is done. v0 is the
// minimal organism: state → context → LLM → tools → edit/command → diff → test
// → event.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/agent/edit"
	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/jobs"
	"github.com/memcode-ai/memcode/internal/agent/mood"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/plan"
	"github.com/memcode-ai/memcode/internal/agent/room"
	"github.com/memcode-ai/memcode/internal/agent/secrets"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/assemble"
	"github.com/memcode-ai/memcode/internal/browser"
	"github.com/memcode-ai/memcode/internal/checkpoint"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/hooks"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/lsp"
	"github.com/memcode-ai/memcode/internal/mcp"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/scripts"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/skills"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/todos"
	"github.com/memcode-ai/memcode/internal/wire"
)

const (
	maxIterations     = 200  // soft cap for ask/auto (and the plan-mode runaway backstop) — a real multi-file task takes many tool rounds; 30 truncated legitimate work mid-task
	maxIterationsYolo = 1000 // allow-all HARD ceiling: the pure runaway/token backstop for long unattended runs (the no-progress stall detector was removed — it killed legitimate long work)
	reviewMaxTurns    = 8    // the tooled plan reviewer is a fast SANITY GATE, not a re-planner — a few targeted reads to spot-check the riskiest claims, then verdict (it was overshooting toward a full re-audit)
	bashTimeout       = 2 * time.Minute
	searchTimeout     = 30 * time.Second // a content search must never hang the turn
	maxParallelTools  = 8                // read-only tool calls in one turn run concurrently, capped
	maxToolOutput     = 8 * 1024
	maxFileRead       = 64 * 1024
	coalesceWindow    = 1500 * time.Millisecond // rapid successive lines merge into one turn
	maxAttachInline   = 8 * 1024
	predictMaxTokens  = 1024 // predict is two short sections; no need for the 4096 default

	// maxRetriesClientVisible mirrors the SDK client's retry bound (sdk-go client.maxRetries).
	// Shown in the "⊙ retrying (n/N)" TUI line so the user sees the retry budget. Kept in sync
	// manually — the client's constant is unexported (the SDK doesn't export its retry policy).
	maxRetriesClientVisible = 3
)

// Session runs one agent task against a project.
type Session struct {
	store             store.Store
	runner            *llm.Runner            // metered model gateway (this session's own Fork; shares the central Ledger)
	prov              provider.ModelProvider // raw provider, ONLY for capability checks (WebSearcher/WebFetcher/doctor)
	purpose           llm.Purpose            // ledger purpose for THIS session's main loop (main_loop, or explore for scout sub-agents)
	root              string
	model             string
	scoutModel        string // model for read-only explore sub-agents (cheap; Luna by default)
	vendor            string // per-session strong-tier vendor ("" = configured default; set by /model)
	pin               string // pinned model label ("" = Automatic; set by /model — every real request serves this model)
	pinWindow         int    // the pin's context window from the picker list (0 = unknown; sizes the meter before the first serve)
	mode              permissions.Mode
	modeMu            sync.RWMutex // guards mode: the TUI goroutine cycles it (Shift+Tab // /mode) while the engine reads it at the permission gate
	personality       string       // chosen voice (built-in key or custom text); travels as a fact, tone-only
	personalityRoll   string       // the concrete voice "random" rolled for THIS session (set in SetPersonality)
	extraMile         bool         // "extra mile" mode: travels as a fact; the doctrine composer injects an above-and-beyond rule for planner+executor
	effortOverride    wire.Effort  // user-forced thinking effort (/effort); applied when hasEffortOverride is set
	hasEffortOverride bool         // false → auto (the turn_intent judge decides per turn); true → use effortOverride every turn
	out               io.Writer
	width             int // terminal width, for full-row diff backgrounds (0 → fallback)
	approve           func(context.Context, ApprovalRequest) ApprovalDecision
	ask               func(context.Context, AskRequest) AskResponse // HITL clarifying questions
	mu                sync.Mutex                                    // guards output + metric counters during concurrent tool execution, and planCtl.Task/LastPlan for the plan-gate snapshot (PlanGateSnapshot / pinPresentedPlan)
	browserRenderOK   bool                                          // user consented to local browser rendering this session
	browserEnabled    bool                                          // --chrome: browser tools are advertised and a Chrome session is lazily launched
	browserSession    *browser.Session                              // the persistent Chrome instance (lazily created on first browser tool call)
	noContext         bool                                          // cold mode: skip the ContextPack (for A/B evaluation)
	readOnly          bool                                          // explorer mode: no edit_file/bash (a "reader" sub-agent)
	forceEscalate     bool                                          // strong-tier agent: pin every request to the strong vendor (balanced tier)
	forceFrontier     bool                                          // long-running (background) agent: pin every request to the FRONTIER tier
	iterCap           int                                           // per-session runLoop iteration override (0 = mode default); set on the bounded plan-review sub-session
	scope             string                                        // scout/explorer subsystem scope (tags gather telemetry)
	lastText          string                                        // most recent assistant text (for Answer)
	lastErr           error                                         // terminal error of the most recent turn (for one-shot exit codes)
	sessionID         string
	pinnedID          string                      // caller-chosen session id (SetSessionID): StartChat uses it verbatim instead of minting, for gateway conversation continuity
	headSHA           string                      // repo HEAD at session start — provenance stamp for signals emitted this session
	resumeID          string                      // when set, the next StartChat re-enters this session with its saved transcript (see transcript.go)
	allowPending      string                      // permission-provenance note awaiting its surface's header (see allowNote/flushAllowNote)
	hookSet           *hooks.Set                  // user hooks (~/.memcode/hooks.json + project), lazily loaded; see hooks.go
	ckpt              *checkpoint.Log             // per-session edit pre-images (rewind); recreated with the session id
	curCkpt           *checkpoint.Checkpoint      // the open checkpoint for the RUNNING turn (nil between turns)
	slog              *sessionlog.Writer          // append-only episodic log (.memcode/sessions/<id>/)
	bgJobs            *jobs.Registry              // background jobs ($ … & / agent-started servers)
	mcp               *mcp.Manager                // connected MCP servers + their tools (.mcp.json); nil when none configured
	lspMgr            *lsp.Manager                // resident language servers (diagnostics + nav); lazily started, detect-and-connect
	lspOnce           sync.Once                   // guards lazy lspMgr creation
	mcpPending        []mcp.ScopedServer          // project-scoped servers awaiting approval (reviewed on the first interactive turn)
	mcpConfigs        map[string]mcp.ServerConfig // connected servers' configs (invocation grants key to their hash)
	mcpInteractive    bool                        // this session can complete interactive flows (approval prompts, OAuth browser)
	mcpErrsShown      int                         // count of MCP connect errors already surfaced (so Add doesn't re-print)
	bgCtx             context.Context             // LONG-LIVED ctx for jobs (session-scoped, NOT a turn ctx)
	testEditIntent    bool                        // user explicitly asked to change tests/specs/behavior this turn
	lastUserText      string                      // the user's most recent request (ground truth for the authorization judge)
	redactor          *secrets.Redactor
	approvals         []permissions.Approval // remembered allow-rules
	observer          UIObserver             // optional front-end tap (TUI); may be nil
	mood              *mood.Tracker          // running interaction-friction reading
	cadence           *mood.CadenceTracker   // message timing (burst / rapid-correction)
	room              room.State             // assessed interaction/room state (drives policy)
	todos             todos.List             // the agent's in-memory work tracker (scratchpad)

	planCtl *plan.Controller // plan-mode state (active/revision/models/apply), owned by EnterPlan/ExitPlan (plan.go)

	metrics           metricsState     // session accounting counters (tool calls, reads, edit/verify seqs) (metricsstate.go)
	served            servedState      // backend/routing telemetry of the last main call (servedstate.go)
	dispMu            sync.Mutex       // guards served + turnEffort: the TUI render goroutine reads them every frame while the engine writes them mid-turn (separate from mu so a render read never blocks on output I/O)
	turnEffort        wire.Effort      // thinking effort for THIS turn (set per turn; default off — see turnintent.go) — read under dispMu
	servingDefault    string           // the everyday serving model (gateway cheap lane) shown before any turn runs — read under dispMu
	turnHighRisk      bool             // THIS turn touches a high-blast-radius surface (auth/billing/secrets/destructive) → escalate the backend (see highRiskTurn)
	turn              *turnState       // per-turn loop state, reset each runLoop (turnstate.go)
	skills            []skills.Skill   // discovered skill catalog (own + Claude Code plugins)
	approvedSkills    map[string]bool  // skills the user said "don't ask again" for (loaded from + persisted to .memcode/skill-approvals)
	approvedArtifacts bool             // repo-scoped "don't ask again" for artifact publishing (.memcode/artifact-approvals)
	nudgedSkills      map[string]bool  // skill triggers already nudged this session (nudge once, don't nag) — see skillNudge
	scripts           []scripts.Script // saved reusable command sequences (.memcode/scripts) — see script.go, scripts_prompt.go
	nudgedScripts     map[string]bool  // script slugs already nudged this session (nudge once, don't nag) — see scriptNudge
	userMd            string           // user's MEMCODE.md instructions, loaded once per session, injected every turn
	memoryMd          string           // durable memory (global + project memory.md), loaded once per session, injected every turn
	supplemental      []ContextItem    // caller-supplied supplemental context (empty for the CLI/Desktop; set only by the agent runtime), injected every turn
	extraSkillRoots   []string         // caller-supplied extra skill roots (a gateway persona's skills dir); empty for the CLI/Desktop
	editsAllowed      bool             // user said "don't ask again for edits" this session (scoped: edits only, not commands; never catastrophic)

	lastCompactSummary string // most recent in-session compaction summary (the warm layer)

	// turn_intent judge state (turnintent.go): the in-flight judgment channel
	// (fired in scoreTurn, joined in runLoop), the judged tier verdict stamped
	// onto every request this turn, the judged thinking baseline the reasoning
	// tool's "auto" restores, the previous judgment (continuation inheritance),
	// and the one-shot plan-hint flag. Engine goroutine only.
	turnJudge        chan turnJudgment
	turnDifficulty   string
	turnBaseEffort   wire.Effort
	lastJudgment     turnJudgment
	nudgedPlanIntent bool
	lastCompactAfter int // est tokens right after the last compaction pass — the back-off baseline (skip re-compaction until real regrowth; see compactIfNeeded/manageInTurnContext)

	// hotPaths is the cross-turn HOT working set: read_file paths the session
	// keeps re-reading (fed from each turn's gather counts in runLoop's defer,
	// decayed by half per turn, capped at hotPathsCap). The evictor pins a hot
	// path's latest read so it stops thrashing read→evict→re-read. Engine
	// goroutine only — same discipline as s.turn.
	hotPaths map[string]int

	// liveChat is the interactive session's ChatState, wired by StartChat so that a
	// mid-session model switch (SetPin/SetVendor) can strip provider-specific thinking
	// blocks from the live history. nil in headless Run()/Answer() — those build their own
	// ephemeral ChatState and never switch models mid-flight.
	liveChat *ChatState

	// steerDrain, when set, returns any `+steer` text the user submitted into the ACTIVE
	// transaction while this turn was running; runLoop folds it in at the safe boundary
	// (after tool_results, before the next model call). Nil in headless Run() — no
	// interactive steering there. Wired by the TUI to the scheduler's DrainSteers.
	steerDrain func() []string

	// separateDrain, when set, returns any texts the background follow-up classifier judged
	// SEPARATE from the active task (a disparate follow-up, not a refinement) along with the
	// active task's raw text AND the classifier's synthesized title for it (same call, no
	// extra cost); runLoop tracks them on the todo list and gives the model a brief FYI note
	// at the same safe boundary steers fold in at. Nil in headless Run(). Wired by the TUI to
	// the scheduler's DrainSeparate.
	separateDrain func() (activeText string, activeTitle string, items []separateAsk)

	// planDeferred holds messages the plan-intake classifier judged SEPARATE from the plan
	// being drafted (unrelated to planCtl.Task) — parked here instead of ever reaching the
	// scheduler while planning, so they can't corrupt the plan draft. Drained (FIFO) by
	// DrainPlanDeferred on ExitPlan (Execute or Cancel) and replayed through the scheduler.
	planDeferred []separateAsk

	// judges tracks side-channel classifier outcomes (ok/timeout/err per mode) — the
	// /doctor instrument for silent judge degradation. Own mutex inside; see judge.go.
	judges judgeStats
}

// SetSteerDrain wires the interactive steer source (the transaction scheduler). runLoop
// calls it between tool iterations to fold mid-turn `+steer` input into the active turn.
func (s *Session) SetSteerDrain(f func() []string) { s.steerDrain = f }

// SetSeparateDrain wires the interactive separate-task source (the transaction scheduler).
// runLoop calls it between tool iterations to track genuinely disparate follow-ups on the
// todo list instead of leaving them invisible in the queue.
func (s *Session) SetSeparateDrain(f func() (activeText string, activeTitle string, items []separateAsk)) {
	s.separateDrain = f
}

// Result reports measurable outcomes of a session (used by the A/B harness).
type Result struct {
	SessionID    string   `json:"session_id"`
	Iterations   int      `json:"iterations"`
	ToolCalls    int      `json:"tool_calls"`
	WrongTurns   int      `json:"wrong_turns"` // failed tool calls
	FilesRead    int      `json:"files_read"`
	FilesChanged []string `json:"files_changed"`
	DiffLines    int      `json:"diff_lines"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	Verified     bool     `json:"verified"` // a verification command passed
}

// New constructs a Session.
func New(st store.Store, runner *llm.Runner, root, model string, mode permissions.Mode, out io.Writer) *Session {
	s := &Session{
		store:      st,
		runner:     runner,
		prov:       runner.Provider(), // raw provider, for capability checks only
		purpose:    llm.MainLoop,      // top-level default; explore sub-agents override to llm.Explore
		root:       root,
		model:      model,
		scoutModel: catalog.ModelLuna,
		mode:       mode,
		out:        out,
		approve:    stdinApprover(out),
		ask:        stdinAsker(out),
		redactor:   secrets.NewFromEnv(),
		mood:       mood.NewTracker(),
		cadence:    mood.NewCadenceTracker(),
		bgJobs:     jobs.New(),
		bgCtx:      context.Background(), // replaced with the session ctx in StartChat/Run
		turn:       newTurnState(),       // non-nil default; runLoop replaces it each turn
		planCtl:    &plan.Controller{},
	}
	// Wire the gateway client's retry-notify into the session's output, so transient
	// HTTP failures (Cloud Run cold-start 502, 429, a dropped connection) surface a
	// "⊙ retrying…" line in the TUI instead of killing the turn silently. The provider
	// is the SDK's *client.Client in production; SetRetryNotify is a no-op for any
	// provider that doesn't support it (test fakes, a future local backend).
	provider.SetRetryNotify(s.prov, func(attempt int, err error, delay time.Duration) {
		s.printf("  ⊙ transient gateway error (%v) — retrying (%d/%d, in %s)\n",
			err, attempt, maxRetriesClientVisible, delay.Round(time.Millisecond))
	})
	return s
}

// Connected reports whether the session's provider has a usable backend —
// hosted gateway credentials OR a configured custom endpoint (the Phase C
// widening; the seam is provider.Connector). Providers without the Connector
// capability (test fakes, futures) count as connected — the mandatory-login
// gate only applies to the real lazy client.
func (s *Session) Connected() bool {
	if c, ok := s.prov.(provider.Connector); ok {
		return c.Connected()
	}
	return true
}

// Endpoint reports the ACTIVE custom endpoint when the session runs against an
// arbitrary OpenAI-compat backend instead of the memcode gateway (one-wire
// Phase C). ok=false on the hosted gateway, signed out, and providers without
// the Endpointer capability (test fakes). The TUI keys the /model picker, cost
// display, and login-card copy on this; the runtime keys tool gating on it.
func (s *Session) Endpoint() (provider.Endpoint, bool) {
	if e, ok := s.prov.(provider.Endpointer); ok {
		return e.Endpoint()
	}
	return provider.Endpoint{}, false
}

// endpointMode reports the session serves from a custom endpoint — the
// capability-gating signal (no memcode side channels, no server-side search).
func (s *Session) endpointMode() bool {
	_, ok := s.Endpoint()
	return ok
}

// SetCredentials swaps fresh gateway credentials into the provider (the
// /login success path — no restart). No-op without the Connector capability.
func (s *Session) SetCredentials(url, token string) {
	if c, ok := s.prov.(provider.Connector); ok {
		c.SetCredentials(url, token)
	}
	s.InvalidateModels() // fresh org context → fresh control-plane snapshot
}

// InvalidateModels drops the Runner's control-plane snapshot (roles, byok
// coverage, credits state) so the next model call refetches — the hook for
// /login, /apikeys mutations, and anything else that changes selection inputs.
func (s *Session) InvalidateModels() {
	if s.runner != nil {
		s.runner.InvalidateModels()
	}
}

// ClearCredentials disconnects the provider (the /logout path).
func (s *Session) ClearCredentials() {
	if c, ok := s.prov.(interface{ ClearCredentials() }); ok {
		c.ClearCredentials()
	}
	s.InvalidateModels()
}

// AddRedactSecrets registers additional secret values with the session's
// redactor (e.g. the token /login just received) so they never reach
// transcripts, traces, or tool output.
func (s *Session) AddRedactSecrets(values ...string) { s.redactor.Add(values...) }

// SetNoContext puts the session in cold mode (no ContextPack) — for A/B eval.
func (s *Session) SetNoContext(v bool) { s.noContext = v }

// SetBrowserEnabled enables the browser tools (--chrome). When enabled, the six
// browser tools are advertised to the model and a persistent Chrome session is
// lazily launched on the first browser tool call. Chrome always opens with a
// visible window (headed) — you can watch it work. The Chrome process is torn
// down on CloseBrowser (called at session end).
func (s *Session) SetBrowserEnabled(enabled bool) {
	s.browserEnabled = enabled
}

// BrowserEnabled reports whether --chrome is active (browser tools are advertised
// and a Chrome session may be launched). Used by the TUI's /dispatch to forward
// the capability to spawned sub-agents.
func (s *Session) BrowserEnabled() bool {
	return s.browserEnabled
}

// CloseBrowser tears down the Chrome process if one was launched. Safe to call
// when no browser session exists (nil-safe). Called at session end.
func (s *Session) CloseBrowser() {
	if s.browserSession != nil {
		s.browserSession.Close()
		s.browserSession = nil
	}
}

// SetWidth records the terminal width so rendered diffs can fill the full row
// with a background. The TUI calls this on resize.
func (s *Session) SetWidth(w int) { s.width = w }

// diffWidth is the terminal width the diff renderer lays out within. diffRow leaves a
// black margin on BOTH sides (diffMargin), so the colored band is printed at
// width-diffMargin cells and never reaches the right-edge wrap boundary — no need to
// shave a column here. Falls back to a sane default before the first WindowSizeMsg.
func (s *Session) diffWidth() int {
	if s.width <= 8 {
		return 95
	}
	return s.width - 1 // width-1: keep styled diff rows under the scrollback wrap limit so they
	// aren't re-split (and never sit on the terminal's exact-width auto-wrap boundary).
}

// setSessionID assigns this Session's id AND ties the metered Runner to it, so
// every model call carries the session on the wire (the compat `user` field) for
// serving affinity + telemetry. Use this everywhere instead of writing
// s.sessionID directly.
// SetSessionID pins the session id used by the next StartChat, so a caller can
// control continuity itself: the gateway derives a stable id per conversation and
// pins it, and StartChat then does resume-or-create under that id instead of
// minting a fresh one. (Distinct from a leftover sessionID between two chats on
// one Session, which must still mint a new id.)
func (s *Session) SetSessionID(id string) { s.pinnedID = id }

// SetContext supplies caller-provided supplemental context for the run (agent
// runtime, API, CI). Injected every turn after project/user context, in the
// engine's fixed Kind order. The CLI and Desktop never call this, so their
// context is unchanged. Supplemental context is input for this run only — it does
// not write project memory.
func (s *Session) SetContext(items []ContextItem) { s.supplemental = items }

// SetSkillRoots supplies caller-provided EXTRA skill discovery roots (e.g. a
// gateway persona's own skills dir). They rank between repo-local and
// user-global skills, so a persona can carry capabilities without editing the
// project or the user's global skill set. Empty for the CLI and Desktop.
func (s *Session) SetSkillRoots(roots []string) { s.extraSkillRoots = roots }

func (s *Session) setSessionID(id string) {
	s.sessionID = id
	s.ckpt = checkpoint.New(s.root, id) // rewind points live per session id
	if s.runner != nil {
		s.runner.SetSession(id)
	}
}

// Run executes a task end-to-end and returns its metrics.
func (s *Session) Run(ctx context.Context, task string) (Result, error) {
	s.setSessionID(newSessionID())
	s.curCkpt = s.ckpt.Begin(task)                 // rewind point for the one-shot's edits
	s.bgCtx = ctx                                  // long-lived: background jobs survive the agent's turns
	s.testEditIntent = userIntendsTestChange(task) // did the task authorize changing tests/specs?
	s.lastUserText = task                          // ground truth for the authorization judge
	defer s.KillAllJobs()                          // a one-shot headless run must not leave a server orphaned
	s.approvals = s.loadApprovals(ctx)

	// Clean baseline: record HEAD and the already-dirty files so we can attribute
	// only the agent's own changes (not the pre-existing working tree).
	headSHA := gitHead(ctx, s.root)
	dirtyBefore := changedFiles(ctx, s.root)
	beforeHashes := s.hashSet(dirtyBefore) // to detect further edits to pre-dirty files
	s.emit(ctx, events.KindAgentSessionStarted, map[string]any{
		"task": task, "mode": string(s.mode), "model": s.model,
		"head_sha": headSHA, "dirty_before": dirtyBefore})
	s.startSessionLog(headSHA) // open the append-only episodic log
	s.logUser(input.Bundle{Text: task})
	defer s.endSessionLog()

	// Orient. Normal mode embeds the (redacted) ContextPack in the system prompt;
	// cold mode (A/B eval) omits it.
	var sys promptSpec
	if s.noContext {
		s.printf("● cold mode: no context pack\n")
		sys = s.coldSpec()
	} else {
		pack, err := assemble.Context(ctx, s.store, s.root, task)
		if err != nil {
			return Result{}, err
		}
		packJSON, _ := json.MarshalIndent(pack, "", "  ")
		sys = s.execSpec(s.redactor.Redact(string(packJSON)))
	}
	s.skills = skills.DiscoverIn(s.root, s.extraSkillRoots) // recruitable skills for this headless run too
	s.approvedSkills = loadApprovedSkills(s.root)           // honor remembered "don't ask again" skill approvals
	s.approvedArtifacts = loadArtifactApproval(s.root)      // ditto for artifact publishing
	s.nudgedSkills = map[string]bool{}                      // per-session: a matched skill is nudged once
	if list, err := scripts.List(s.root); err == nil {
		s.scripts = list // saved reusable command sequences for this headless run too
	}
	s.nudgedScripts = map[string]bool{} // per-session: a matched script is nudged once
	s.connectMCP(ctx, false)            // connect .mcp.json servers (headless: no prompts/OAuth)
	defer s.closeMCP()
	// Point at the skill dirs (don't dump blurbs) — the model greps/reads on demand and
	// recruits any discovered skill by name via the skill tool.
	if ptr := skillsPointer(skills.RootsIn(s.root, s.extraSkillRoots)); ptr != "" {
		sys = sys.withExtra(ptr)
	}
	if ptr := scriptsPointer(s.scripts); ptr != "" {
		sys = sys.withExtra(ptr)
	}
	if kp := knowledgePointer(s.root); kp != "" { // name the built-in knowledge packs (lead with detected stacks)
		sys = sys.withExtra(kp)
	}
	if s.userMd = s.userInstructions(ctx); s.userMd != "" { // MEMCODE.md / CLAUDE.md standing instructions
		sys = sys.withExtra(s.userMd)
	}
	if s.memoryMd = s.userMemory(ctx); s.memoryMd != "" { // durable memory (global + project), facts not rules
		sys = sys.withExtra(s.memoryMd)
	}
	if blk := supplementalBlock(s.supplemental); blk != "" { // caller-supplied context (agent runtime); empty for CLI
		sys = sys.withExtra(blk)
	}
	if nudge := s.skillNudge(task); nudge != "" { // the task names an installed skill → point right at it
		sys = sys.withExtra(nudge)
	}
	if nudge := s.scriptNudge(task); nudge != "" { // the task names a saved script → point right at it
		sys = sys.withExtra(nudge)
	}

	messages := []wire.Message{{
		Role:   "user",
		Blocks: []wire.Block{{Type: "text", Text: task}},
	}}

	// Headless has no room assessment — the turn_intent judge classifies the task
	// synchronously (a background "audit the repo" is exactly the deep case);
	// the ladder resolves the model from the turn's intent.
	s.judgeTurnSync(ctx, task)
	s.turnHighRisk = highRiskTurn(task)
	iterations, completed, err := s.runLoop(ctx, sys, &messages)
	if err != nil {
		return Result{}, err
	}

	// Attribute the agent's changes: newly-dirty files plus pre-dirty files whose
	// content the agent changed further.
	changed := newlyChanged(dirtyBefore, changedFiles(ctx, s.root))
	for p, h0 := range beforeHashes {
		if h1, ok := edit.Hash(s.root, p); ok && h1 != h0 {
			changed = append(changed, p) // disjoint from newlyChanged (p was already dirty)
		}
	}

	// Verified only counts if a verification PASSED after the last edit.
	verified := s.metrics.lastVerifyOKSeq > s.metrics.lastEditSeq

	diffStat := gitDiffStat(ctx, s.root, changed)
	usage := s.runner.Ledger().Total()
	s.emit(ctx, events.KindAgentSessionFinished, map[string]any{
		"iterations":             iterations,
		"head_sha":               headSHA,
		"files_changed_by_agent": changed,
		"diff_summary":           diffStat,
		"input_tokens":           usage.In,
		"output_tokens":          usage.Out,
		"cache_read_tokens":      usage.CacheRead,
		"cache_write_tokens":     usage.CacheWrite,
		"est_cost_usd":           usage.USD,
		"verified":               verified,
		"hit_limit":              !completed,
	})

	// Only surface an outcome line when there's something to say: files changed
	// (with the list) or the verification guardrail tripped. A clean no-op run
	// prints nothing — the agent's own reply is the whole output.
	if len(changed) > 0 {
		s.printf("\n  agent changed %d file(s): %s\n", len(changed), strings.Join(changed, ", "))
	}
	// Guardrail: a change must be verified AFTER the last edit, not merely sometime.
	if s.metrics.didEdit && !verified {
		s.printf("  ⚠ files changed but not verified after the last edit\n")
	}

	return Result{
		SessionID:    s.sessionID,
		Iterations:   iterations,
		ToolCalls:    s.metrics.toolCalls,
		WrongTurns:   s.metrics.toolErrors,
		FilesRead:    s.metrics.filesRead,
		FilesChanged: changed,
		DiffLines:    gitDiffLines(ctx, s.root, changed),
		InputTokens:  usage.In,
		OutputTokens: usage.Out,
		Verified:     verified,
	}, nil
}

// gate applies the permission policy for a non-command action (e.g. an edit).
// It returns whether to proceed and, when denied, a reason to feed back to the
// model so it can adjust rather than just seeing "denied".
func (s *Session) gate(ctx context.Context, risk permissions.Risk, catastrophic bool, req ApprovalRequest) (bool, string) {
	// Session-scoped "don't ask again for edits" grant (the card's Remember on an
	// edit). Scoped to EDITS — commands stay gated by mode — and never covers a
	// catastrophic edit (e.g. self-heal weakening a test); that floor is deterministic.
	if s.editsAllowed && !catastrophic {
		return true, ""
	}
	switch permissions.Decide(s.effectiveMode(), risk, catastrophic) {
	case permissions.Allow:
		return true, ""
	case permissions.NeedPrompt:
		d := s.askApproval(ctx, req)
		if !d.Allow {
			return false, orEmpty(d.Reason, "denied by user")
		}
		// Remember on an edit = "stop asking for edits this session" (NOT a global
		// allow-all, NOT a command rule). Never persisted for catastrophic edits.
		if d.Remember && !catastrophic {
			s.editsAllowed = true
			s.printf("  ✓ won't ask again for edits this session\n")
		}
		return true, ""
	default:
		return false, "blocked by permission policy"
	}
}

// askApproval runs the approver and applies the shared bookkeeping: an interrupt
// stops the turn; any non-allow is recorded as a trust signal.
func (s *Session) askApproval(ctx context.Context, req ApprovalRequest) ApprovalDecision {
	d := s.approve(ctx, req)
	s.logApproval(req, d) // crisp "what did the user approve/deny" trail
	if d.Interrupt {
		s.turn.interrupted = true
		s.emit(ctx, events.KindInputInterrupted, map[string]any{"during": req.Label})
	}
	if !d.Allow {
		s.noteDenied(ctx, req.Title)
	}
	return d
}

// effectiveMode returns the permission mode that gates writes. The room/friction
// signal is ADVISORY ONLY — it drives narration, pacing, and model routing (see
// route.go frictionEscalates: friction → escalate to a stronger model, i.e. "think
// harder"), but it must NEVER tighten an explicit auto/allow-all into ask. allow-all
// is the user's explicit "stop asking" directive; a mood heuristic flipping it back to
// ask is the "eggshells" failure mode (scared to act right when the user wants decisive
// action). The real floors are deterministic and enforced independently: the
// catastrophic flag passed to permissions.Decide, and the authorization judge in
// gateCommand (which already excludes allow-all). Receiving feedback is a cue to act
// decisively on it — not to freeze.
func (s *Session) effectiveMode() permissions.Mode {
	// Plan mode is a read-only inspect shell: auto-run only Safe commands; anything
	// that may mutate must be approved — even under auto/allow-all.
	if s.planCtl.Planning() {
		return permissions.ModeAsk
	}
	s.modeMu.RLock()
	defer s.modeMu.RUnlock()
	return s.mode
}

// noteDenied records a declined action as a trust signal and nudges friction up.
func (s *Session) noteDenied(ctx context.Context, action string) {
	s.emit(ctx, events.KindActionDenied, map[string]any{"action": action})
	if s.mood != nil {
		s.mood.Bump(0.2, "approval-denied")
	}
}

// NoteFriction lets a front-end fold in a non-textual friction signal it alone
// can see — a rapid Esc-mash ("rage keys") — and returns the updated reading so
// the gauge can refresh.
func (s *Session) NoteFriction(signal string, amount float64) mood.Reading {
	return s.mood.Bump(amount, signal)
}

// Room returns the current assessed room state (for the TUI badge / tests).
func (s *Session) Room() room.State { return s.room }

// LastText returns the most recent assistant message — e.g. the just-proposed plan —
// so the front-end can hand it to the (stateless) advisor for review. The advisor has
// no session access, so without this the plan would never reach it.
func (s *Session) LastText() string { return s.lastText }

// LastError returns the terminal error of the most recent turn (nil on success). Meaningful
// on the synchronous one-shot path (agent -c / resumed one-shot), where a non-nil result must
// surface as a non-zero exit code for scripting/CI.
func (s *Session) LastError() error { return s.lastErr }

// rootIsGit reports whether the session root is a git repo (a .git dir or, for worktrees, a .git
// file). The recoverability downgrade gates on this: without git there's no restore net, so a
// destructive op isn't actually recoverable. Cheap stat; only hit when a destructive command gates.
func (s *Session) rootIsGit() bool {
	_, err := os.Stat(filepath.Join(s.root, ".git"))
	return err == nil
}

// lsp returns the session's resident Language Server manager, created on first use
// (detect-and-connect: individual servers start lazily and only when their binary is on
// PATH). Read-only sub-agents share the parent's manager via the fork path; a bare
// Session with no root still gets a manager that simply reports every language
// unsupported.
func (s *Session) lsp() *lsp.Manager {
	s.lspOnce.Do(func() { s.lspMgr = lsp.NewManager(s.root) })
	return s.lspMgr
}

// allowNote records permission PROVENANCE: why a command ran without a prompt
// (remembered rule, mode classifier, explicit authorization). It is PENDING
// until the surface prints its own header (the ⏺ Bash marker, the $ echo) and
// flushes — so the note attaches to the block it explains instead of floating
// above the previous one.
func (s *Session) allowNote(reason string) { s.allowPending = reason }

// flushAllowNote prints the pending provenance note as a muted ⎿ sub-line.
func (s *Session) flushAllowNote() {
	if s.allowPending == "" {
		return
	}
	s.printf("%s\n", metaStyle.Render("  ⎿ "+s.allowPending))
	s.allowPending = ""
}

// gateCommand applies the policy to a shell command, honoring remembered
// approvals before falling back to the mode/prompt. It returns whether to run,
// the command to actually run (the user may substitute a safer one), and a deny
// reason for the model when refused.
func (s *Session) gateCommand(ctx context.Context, risk permissions.Risk, catastrophic bool, command, cwd string) (bool, string, string) {
	if _, ok := permissions.Match(s.approvals, command, cwd, catastrophic, time.Now()); ok {
		// Permission provenance, TERSE: one word of trust signal, not a policy essay.
		s.allowNote("pre-approved")
		return true, command, ""
	}
	// RECOVERABILITY downgrade: a destructive op whose blast radius is confined to THIS git repo is
	// recoverable (git restores it), so gate it like the edit tool — Medium — not as an always-confirm
	// catastrophe. This fixes the inconsistency the user hit: edit_file deletes/overwrites a file at
	// Medium (auto-runs in auto), but `rm <the same file>` was catastrophic — which always prompts in
	// EVERY mode, so a routine in-repo cleanup nagged endlessly.
	// Only when root is actually a git repo (else there's no recovery net); out-of-repo / remote /
	// disk-level ops keep their floor (RecoverableInRepo is conservative).
	if (catastrophic || risk > permissions.Medium) && s.rootIsGit() && permissions.RecoverableInRepo(command, cwd, s.root) {
		risk, catastrophic = permissions.Medium, false
		if s.effectiveMode() != permissions.ModeAsk {
			// auto / allow-all / plan-apply: runs unprompted, like an edit
			s.allowNote("auto-allowed")
			return true, command, ""
		}
		// ask mode still prompts (ask prompts for any write) — but it's Medium now, so the
		// card's "don't ask again" saves an untrusted (ordinary) rule.
	}
	base := permissions.Decide(s.effectiveMode(), risk, catastrophic)
	// AUTHORIZATION judge (the overeager-agent defense, distinct from risk): for a
	// mutating, non-catastrophic command in the interactive loop, ask whether the
	// USER authorized THIS action (vs the agent freelancing toward the goal). It can
	// ESCALATE an auto-allowed-but-unauthorized action to a prompt, or DOWNGRADE a
	// prompt to allow on explicit authorization (less fatigue). Never touches
	// catastrophic (always prompts) or read-only/plan/allow-all contexts.
	allowReason := ""
	if base == permissions.Allow && risk >= permissions.Medium {
		allowReason = "auto-allowed" // would have prompted in ask mode
	}
	// Plan APPLY is exempt: the user just approved a written plan naming this work — that
	// approval IS the authorization, and the judge second-guessing the plan's own steps
	// (`gofmt` + the plan's verification commands) is prompt fatigue with no signal.
	if base != permissions.Block && !catastrophic && risk >= permissions.Medium &&
		!s.readOnly && !s.planCtl.Planning() && !s.planCtl.IsApplying() && s.effectiveMode() != permissions.ModeAllowAll {
		switch d, why := s.authorizeCommand(ctx, command, s.lastUserText, cwd); d {
		case "block":
			return false, "", "not authorized: " + orEmpty(why, "the user did not authorize this action")
		case "ask":
			base = permissions.NeedPrompt
		case "allow":
			base = permissions.Allow
			allowReason = "auto-allowed"
		}
	}
	switch base {
	case permissions.Allow:
		// Safe reads run silently (attribution on every ls would be spam); anything
		// that would have prompted in ask mode names the gate that let it through.
		if allowReason != "" {
			s.allowNote(allowReason)
		}
		return true, command, ""
	case permissions.NeedPrompt:
		d := s.askApproval(ctx, ApprovalRequest{
			Title: command, Label: "Bash command", Detail: cwd,
			Command: command, Cwd: cwd, Editable: true, Risk: risk.String(),
		})
		if !d.Allow {
			return false, "", orEmpty(d.Reason, "denied by user")
		}
		// Honor the user's "don't ask again" UNCONDITIONALLY — the card offered it,
		// the user chose it. It used to be silently dropped for catastrophic
		// commands (the card promised "won't ask again for rm", saved nothing, and
		// re-prompted the very next rm). A rule born from a catastrophic prompt is
		// saved trusted so Match honors it for catastrophic commands too.
		if d.Remember {
			s.rememberApproval(ctx, command, catastrophic)
		}
		run := command
		if c := strings.TrimSpace(d.Command); c != "" && c != command {
			run = c
			s.printf("  ✎ running your command instead: %s\n", run)
		}
		return true, run, ""
	default:
		return false, "", "blocked by permission policy"
	}
}

// rememberApproval appends a "don't ask again" rule to the editable
// .memcode/permissions file and reloads the in-memory rules so it takes effect
// immediately AND persists into future sessions. The pattern is binary-scoped
// (see rememberPattern). trusted mirrors the prompt that spawned the rule: a
// rule remembered FROM a catastrophic prompt (rm/…) is saved trusted — the user
// explicitly said "don't ask again for rm", and if a user allows rm, rm is
// allowed (silently discarding that choice was the "it keeps asking about the
// same rm" bug) — while a rule from an ordinary prompt stays untrusted, so a
// remembered "find *" still can't auto-approve a future catastrophic compound.
func (s *Session) rememberApproval(ctx context.Context, command string, trusted bool) {
	pat := rememberPattern(command)
	if err := permissions.Append(s.root, pat, trusted); err != nil {
		s.printf("  ⚠ couldn't save approval: %v\n", err)
		return
	}
	s.approvals = s.loadApprovals(ctx)
	s.printf("  ✓ won't ask again for %s (saved to .memcode/permissions)\n", pat)
}

// firstWord returns a command's binary — its leading token, SKIPPING leading inline
// env-var assignments (`VAR=value cmd …`). Without the skip, `SDK=/path grep …` was
// treated as the binary `SDK=/path` and remembered as the glob `SDK=/path *`, a
// degenerate rule that auto-approves anything with that exact prefix (seen in the wild).
func firstWord(command string) string {
	for _, f := range strings.Fields(strings.TrimSpace(command)) {
		if isEnvAssignment(f) {
			continue // VAR=value prefix — not the command
		}
		return f
	}
	return ""
}

// isEnvAssignment reports whether tok is a leading shell env assignment (NAME=value):
// a valid identifier (letters/digits/underscore, not starting with a digit) before the
// first '='.
func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := tok[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// rememberPattern derives the approval glob for "don't ask again": "<binary> *",
// matching the card's promise ("don't ask again for <binary> commands"). This
// intentionally covers pipelines that START with the binary (find … | grep …);
// the safety net is that a rule saved from an ORDINARY prompt is untrusted and
// Match requires a Trusted rule for catastrophic commands — so a remembered
// "find *" can't auto-approve "find … | rm -rf …". Only a rule the user saved
// FROM a catastrophic prompt (keyed on that catastrophic binary, e.g. "rm *")
// is trusted to cover its own kind.
func rememberPattern(command string) string {
	// Key on the binary that DROVE the escalation, not the leading token. For a compound
	// `echo … && supabase status` the prompt exists because of `supabase`; keying on the
	// leading `echo` saved a useless+unsafe "echo *" (it auto-approved every echo-led
	// compound while never covering the supabase command the user actually approved).
	// RiskHead returns the highest-risk sub-command's binary (env-assignments + $(...)
	// stripped) and falls back to the leading head for all-safe / single commands.
	if h := permissions.RiskHead(command); h != "" {
		return h + " *"
	}
	return strings.TrimSpace(command)
}

// loadApprovals reads remembered approval rules from the editable
// .memcode/permissions file.
func (s *Session) loadApprovals(context.Context) []permissions.Approval {
	approvals, err := permissions.Load(s.root)
	if err != nil {
		// A corrupt approvals file fails CLOSED (no remembered allows → re-prompt), which is
		// safe — but say so, instead of silently re-prompting all session with no clue why.
		// (An ABSENT file is not an error; permissions.Load returns nil,nil for that.)
		s.printf("⚠ couldn't load saved approvals (%v) — you'll be asked to confirm actions this session\n", err)
		return nil
	}
	return approvals
}

// printf writes to the session output under a lock so concurrent read-only tool
// calls (executed in parallel within a turn) never interleave mid-line.
func (s *Session) printf(format string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.out, format, a...)
}

// startSessionLog opens the append-only episodic log for the current session and
// records the opening event. Failures degrade silently — a nil Writer is safe.
func (s *Session) startSessionLog(headSHA string) {
	w, err := sessionlog.Open(s.root, s.sessionID)
	if err != nil {
		// No episodic log this session — recall across compaction degrades silently
		// otherwise, so tell the user rather than letting it vanish.
		s.printf("⚠ episodic session log unavailable (%v) — cross-compaction recall will be limited this session\n", err)
		return
	}
	s.slog = w
	s.slog.Append(sessionlog.Record{Kind: sessionlog.KindSessionStarted, Model: s.model, Mode: string(s.mode), HeadSHA: headSHA})
}

// endSessionLog records the closing event and closes the log.
func (s *Session) endSessionLog() {
	if s.slog == nil {
		return
	}
	s.slog.Append(sessionlog.Record{Kind: sessionlog.KindSessionFinished})
	_ = s.slog.Close()
	s.slog = nil
}

// logUser records every user turn in the episodic log (redacted). Canonical text
// takes precedence; the marker preserves a manually-constructed attachment-only
// bundle without putting attachment bytes into durable history.
func (s *Session) logUser(b input.Bundle) {
	if s.slog == nil {
		return
	}
	if text := userTurnLogText(b); text != "" {
		s.slog.Append(sessionlog.Record{Kind: sessionlog.KindUserMessage, Text: s.redactor.Redact(text)})
	}
}

func userTurnLogText(b input.Bundle) string {
	if strings.TrimSpace(b.Text) != "" {
		return b.Text
	}
	if b.AttachmentOnly || len(b.Attachments) > 0 {
		if len(b.Attachments) == 0 {
			return "[attachment-only input]"
		}
		// Count per kind, deterministically ordered, so history says WHAT arrived
		// ("2 image, 1 pdf") without storing bytes or relying on paths.
		counts := map[string]int{}
		for _, a := range b.Attachments {
			counts[string(a.Kind)]++
		}
		kinds := make([]string, 0, len(counts))
		for k := range counts {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		parts := make([]string, 0, len(kinds))
		for _, k := range kinds {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
		return "[attachment-only input: " + strings.Join(parts, ", ") + "]"
	}
	return ""
}

// logShell records a `$` direct-shell command in the episodic log as a tool call —
// command, output, and exit — so it shows up in recap/commits/search AND can be
// recalled as the "last shell result" for explain/fix-last. Tool "shell" marks it as
// the user's direct lane (vs the agent's "bash" tool); commit/test detection keys on
// Input, so this still surfaces in Commits().
func (s *Session) logShell(command, output string, exitCode int) {
	if s.slog == nil || strings.TrimSpace(command) == "" {
		return
	}
	s.slog.Append(sessionlog.Record{
		Kind:    sessionlog.KindToolCall,
		Tool:    "shell",
		Input:   s.redactor.Redact(command),
		Content: s.redactor.Redact(truncate(output, shellLogMax)),
		IsError: exitCode != 0,
		Exit:    exitCode,
	})
}

// shellLogMax caps the `$` output kept in the episodic log — enough for the agent to
// explain/fix a failure, without bloating the log with a huge build dump.
const shellLogMax = 4000

// recordTurn appends one model turn to the episodic log: any assistant text and
// each tool/command ACTION (redacted) — the high-level trail you see in the chat,
// not the verbose tool output. This is what the agent self-recalls from.
func (s *Session) recordTurn(text string, uses []wire.Block) {
	if s.slog == nil {
		return
	}
	if strings.TrimSpace(text) != "" {
		s.slog.Append(sessionlog.Record{Kind: sessionlog.KindAssistantMessage, Text: s.redactor.Redact(text)})
	}
	for _, u := range uses {
		if !loggableTool(u.Name) {
			continue // internal reads/searches are noise, not "what happened"
		}
		s.slog.Append(sessionlog.Record{Kind: sessionlog.KindToolCall, Tool: u.Name, Input: s.redactor.Redact(string(u.Input))})
	}
}

// loggableTool reports whether a tool call is a meaningful action worth keeping in
// the canonical session log. Read-only introspection (file reads, greps, the
// memcode read tool, diffs) is mechanics, not memory — it stays out so the log
// reconstructs "what happened" rather than becoming a stdout landfill.
func loggableTool(name string) bool {
	switch name {
	case tools.ReadFile, tools.ListDir, tools.Ripgrep, tools.GitDiff, tools.Memcode:
		return false
	}
	return true
}

// logApproval records an approval decision — a crisp, high-level "what did the
// user approve/deny" trail (no tool output, just the action + outcome).
func (s *Session) logApproval(req ApprovalRequest, d ApprovalDecision) {
	if s.slog == nil {
		return
	}
	subject := req.Command
	if subject == "" {
		subject = req.Title
	}
	verb := "denied"
	switch {
	case d.Interrupt:
		verb = "interrupted"
	case d.Allow && d.Remember:
		verb = "approved (remembered)"
	case d.Allow:
		verb = "approved"
	}
	s.slog.Append(sessionlog.Record{Kind: sessionlog.KindApproval, Decision: verb, Text: s.redactor.Redact(subject)})
}

// emit records an event tagged with this session's id. Telemetry must never take down a
// call path: a session without a store (bare test fixtures) just drops the event.
func (s *Session) emit(ctx context.Context, kind events.Kind, payload map[string]any) {
	if s.store == nil {
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["session_id"] = s.sessionID
	_, _ = events.Append(ctx, s.store, kind, "agent", payload)
}

// Emit is the public alias of emit — the plan.Controller's Session interface needs
// a callable emitter, and interface methods must be exported. Same behavior as emit.
func (s *Session) Emit(ctx context.Context, kind events.Kind, payload map[string]any) {
	s.emit(ctx, kind, payload)
}

// hashSet records content hashes for a set of files (relative to root).
func (s *Session) hashSet(paths []string) map[string]string {
	m := make(map[string]string, len(paths))
	for _, p := range paths {
		if h, ok := edit.Hash(s.root, p); ok {
			m[p] = h
		}
	}
	return m
}

func gitHead(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitDiffLines returns the total added+deleted lines across the given files,
// counting whole-file line counts for new (untracked) files since those don't
// appear in `git diff`.
func gitDiffLines(ctx context.Context, root string, files []string) int {
	total := 0
	for _, f := range files {
		out, err := exec.CommandContext(ctx, "git", "-C", root, "diff", "--numstat", "--", f).Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			fields := strings.Fields(string(out))
			if len(fields) >= 2 {
				if add, e := strconv.Atoi(fields[0]); e == nil {
					total += add
				}
				if del, e := strconv.Atoi(fields[1]); e == nil {
					total += del
				}
				continue
			}
		}
		// Untracked/new file: count its lines.
		if data, e := os.ReadFile(filepath.Join(root, f)); e == nil {
			total += strings.Count(string(data), "\n")
		}
	}
	return total
}

// GitStat is the working-tree summary surfaced in the footer cockpit: how many
// files are dirty and the +/- line churn, split into unstaged (working tree, incl.
// untracked) and staged (index). A clean tree is the zero value.
type GitStat struct {
	Files, Added, Removed                   int // unstaged working tree + untracked
	StagedFiles, StagedAdded, StagedRemoved int // index (git add'd)
}

// Clean reports whether the working tree has nothing uncommitted.
func (g GitStat) Clean() bool {
	return g.Files == 0 && g.StagedFiles == 0 && g.Added == 0 && g.Removed == 0 && g.StagedAdded == 0 && g.StagedRemoved == 0
}

// GitStat computes the current working-tree diffstat. It shells out to git, so the
// TUI must call it OFF the render path (a tea.Cmd), cache the result, and re-run it
// on a throttle / at turn end — never per frame.
func (s *Session) GitStat(ctx context.Context) GitStat {
	var g GitStat
	g.Files, g.Added, g.Removed = numstat(ctx, s.root, false)
	// Untracked files don't appear in `git diff` — count them and their lines. Resolve the
	// untracked set from ONE `git status --porcelain` (the XY prefix already flags "??"),
	// not a per-file `git status -- <f>` (which was 1 porcelain call PER dirty file — the
	// N+1 footer-refresh spam).
	for _, f := range untrackedFiles(ctx, s.root) {
		g.Files++
		if data, e := os.ReadFile(filepath.Join(s.root, f)); e == nil {
			g.Added += strings.Count(string(data), "\n")
		}
	}
	g.StagedFiles, g.StagedAdded, g.StagedRemoved = numstat(ctx, s.root, true)
	return g
}

// untrackedFiles returns the repo's untracked paths from a single porcelain scan (the
// "??" entries). One git call, not one per file.
func untrackedFiles(ctx context.Context, root string) []string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if !strings.HasPrefix(line, "??") || len(line) < 4 {
			continue
		}
		files = append(files, strings.TrimSpace(line[3:]))
	}
	return files
}

// numstat sums files/added/removed from `git diff [--cached] --numstat`.
func numstat(ctx context.Context, root string, staged bool) (files, added, removed int) {
	args := []string{"-C", root, "diff", "--numstat"}
	if staged {
		args = append(args, "--cached")
	}
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return 0, 0, 0
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		files++
		if a, e := strconv.Atoi(f[0]); e == nil {
			added += a
		}
		if d, e := strconv.Atoi(f[1]); e == nil {
			removed += d
		}
	}
	return files, added, removed
}

func gitDiffStat(ctx context.Context, root string, files []string) string {
	if len(files) == 0 {
		return ""
	}
	args := append([]string{"-C", root, "diff", "--stat", "--"}, files...)
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func newSessionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto entropy unavailable (a broken host): fall back to a nanosecond stamp so two
		// sessions never collide on the same id — uniqueness matters more than secrecy here.
		return "sess_" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "sess_" + hex.EncodeToString(b[:])
}

func short(sha string) string {
	if sha == "" {
		return "(none)"
	}
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// shellCmd builds a command using the platform's shell (POSIX sh elsewhere,
// PowerShell on Windows).
// shellCmd builds the command WITHOUT binding a context — cancellation is handled by
// jobs.RunForeground, which kills the whole process GROUP (exec.CommandContext only
// kills the direct child, leaving grandchildren to hold the pipe and hang Wait).
func shellCmd(command string) *exec.Cmd {
	if stdruntime.GOOS == "windows" {
		return exec.Command("powershell", "-NoProfile", "-Command", command)
	}
	return exec.Command("sh", "-c", command)
}

// shellName reports the shell the agent's bash tool uses on this platform.
func shellName() string {
	if stdruntime.GOOS == "windows" {
		return "PowerShell"
	}
	return "POSIX sh"
}

func looksLikeVerify(cmd string) bool {
	c := strings.ToLower(cmd)
	return looksLikeTest(c) || strings.Contains(c, "build") || strings.Contains(c, "vet") ||
		strings.Contains(c, "lint") || strings.Contains(c, "tsc") || strings.Contains(c, "go run")
}

// changedFiles returns paths with uncommitted modifications OR new untracked
// files (so agent-created files are attributed too).
func changedFiles(ctx context.Context, root string) []string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:]) // strip the XY status prefix
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:] // rename: keep the destination
		}
		files = append(files, path)
	}
	return files
}

// newlyChanged returns files dirty in after but not in before.
func newlyChanged(before, after []string) []string {
	seen := make(map[string]bool, len(before))
	for _, f := range before {
		seen[f] = true
	}
	var out []string
	for _, f := range after {
		if !seen[f] {
			out = append(out, f)
		}
	}
	return out
}

func looksLikeTest(cmd string) bool {
	c := strings.ToLower(cmd)
	return strings.Contains(c, "test") || strings.Contains(c, "vitest") || strings.Contains(c, "jest")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

// clip shortens s to at most n runes with a single-line ellipsis — for inline
// progress markers, where the "(truncated)" note and an embedded newline would be
// noise (the reader can see it's clipped).
func clip(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " ") + "…"
}

func orEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// safeJoin resolves rel under root and rejects paths that escape the repo root
// (e.g. "../../.ssh/id_rsa"). Structured file tools must stay inside the repo;
// bash is the explicit, permission-gated escape hatch. The check is BOTH lexical
// AND symlink-aware: a lexical prefix test alone lets an in-repo symlink (./link → /)
// redirect the real write outside the root, so the deepest existing ancestor is
// resolved and re-checked (the classifier's own targetUnderRoot already does this).
func safeJoin(root, rel string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// Canonicalize the root too (e.g. /tmp → /private/tmp on macOS) so the base is real.
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}
	var p string
	if filepath.IsAbs(rel) {
		// An ABSOLUTE path inside the root is honored as-is. One that is NOT under
		// the root is re-rooted ONLY when the model plausibly meant repo-relative
		// with a leading slash ("/cli/foo.go") — i.e. the re-rooted parent exists.
		// Everything else errors honestly: unconditionally joining under root built
		// "<root>/Users/x/Desktop" — a fabricated path that passed the prefix check
		// and then failed downstream as an inscrutable "fork/exec /bin/sh: no such
		// file or directory" (the chdir errno blamed on the shell binary).
		p = filepath.Clean(rel)
		if real, err := resolveExisting(p); err == nil {
			p = real // canonicalize (e.g. /tmp → /private/tmp) so the prefix check compares real paths
		}
		if p != rootAbs && !strings.HasPrefix(p, rootAbs+string(os.PathSeparator)) {
			rerooted := filepath.Clean(filepath.Join(rootAbs, rel))
			if st, err := os.Stat(filepath.Dir(rerooted)); err == nil && st.IsDir() {
				p = rerooted
			} else {
				return "", fmt.Errorf("path escapes the repo root: %s", rel)
			}
		}
	} else {
		p = filepath.Clean(filepath.Join(rootAbs, rel))
	}
	if p != rootAbs && !strings.HasPrefix(p, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes the repo root: %s", rel)
	}
	// The target may not exist yet (a new file), so resolve symlinks on its longest
	// existing prefix and re-append the tail, then confirm the REAL path stays in root.
	if real, err := resolveExisting(p); err == nil {
		if real != rootAbs && !strings.HasPrefix(real, rootAbs+string(os.PathSeparator)) {
			return "", fmt.Errorf("path escapes the repo root via a symlink: %s", rel)
		}
	}
	return p, nil
}

// resolveExisting resolves symlinks on the longest existing prefix of p and re-appends the
// not-yet-existent tail, so a new file is still validated against its real parent directory.
func resolveExisting(p string) (string, error) {
	cur := p
	var tail []string // deepest-first
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			out := resolved
			for i := len(tail) - 1; i >= 0; i-- {
				out = filepath.Join(out, tail[i])
			}
			return out, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no existing ancestor of %s", p)
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// resolveReadPath resolves a path for READ-ONLY access (read_file, list_dir). An absolute path
// is honored as-is and a relative one resolves against the repo root — so reads can reach
// outside the repo, mirroring `cat`/`ls`. That's deliberate: bash can already read anywhere, so
// repo-scoping reads was friction (an out-of-repo absolute path got silently joined onto root
// and failed as a misleading "not found"), not a real boundary. WRITES stay repo-scoped via
// safeJoin — writing outside the repo should be explicit through gated bash, not edit_file.
func resolveReadPath(root, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		rootAbs = root
	}
	return filepath.Clean(filepath.Join(rootAbs, p))
}
