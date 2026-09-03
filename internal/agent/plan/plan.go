// Package plan holds the plan-mode lifecycle as a real state machine. The
// lifecycle itself is unchanged — idle → research → present → approve → apply,
// driven by /plan, the enter_plan/execute_plan/cancel_plan tools, and the
// approval selector — but it used to live as four exported booleans whose
// transitions were field assignments scattered across runtime (loop.go set
// Presented, chat.go cleared Applying, invariants lived as comments). Every
// wrong-plan/clobber/stale-flag incident traced back to a write the package
// could not guard.
//
// Now the Controller's fields are unexported and its named transitions are the
// only way to move it: the compiler rejects outside writes, illegal states like
// "Active and Applying at once" are unrepresentable, and each transition's
// guard is a table-tested rule instead of a convention.
//
// Transitions return Effects (data) instead of performing side effects: the
// machine never touches the session, the observer, the event log, or the disk.
// The runtime applies Effects AFTER the machine's lock is released, so the lock
// can never wrap a printf/observer callback — the old "don't hold s.mu across
// EnterPlan" deadlock trap is gone structurally, and so is the unlocked
// Task/LastPlan reset race (every read and write now goes through c.mu).
package plan

import (
	"regexp"
	"strings"
	"sync"

	"github.com/memcode-ai/memcode/internal/events"
)

// Phase is the plan lifecycle's single word of truth. Exactly one holds at any
// moment; the zero value is Idle so a zero Controller is out of plan mode.
type Phase int

const (
	Idle        Phase = iota // no plan lifecycle in flight
	Researching              // plan mode: drafting/revising, no approvable plan THIS turn
	Presented                // plan mode: a synthesis landed this turn — the selector may raise
	Applying                 // an approved contract is armed or executing
)

// Planning reports whether the phase is inside plan mode proper (research or
// presented) — the old `Active` boolean.
func (p Phase) Planning() bool { return p == Researching || p == Presented }

func (p Phase) String() string {
	switch p {
	case Researching:
		return "researching"
	case Presented:
		return "presented"
	case Applying:
		return "applying"
	default:
		return "idle"
	}
}

// Effects is what a transition asks the caller to do. The machine returns it
// under no lock obligation on the caller's side; runtime applies it via
// Session.applyPlanEffects after the transition returns.
type Effects struct {
	SetModel    string      // non-empty → sess.SetModel (planner on Enter, saved on Approve/Cancel)
	Emit        events.Kind // non-empty → emit with EmitPayload
	EmitPayload map[string]any
	ClearTodos  bool   // Enter: a new plan is a new unit of work — drop the prior checklist
	SavePlan    string // Present, only when the pin was actually replaced (durable ~/.memcode/plans copy)
}

// Zero reports a no-effect result — the signature of a refused (no-op) transition.
func (e Effects) Zero() bool {
	return e.SetModel == "" && e.Emit == "" && !e.ClearTodos && e.SavePlan == "" && len(e.EmitPayload) == 0
}

// PresentOutcome reports whether Present replaced the pinned contract. A
// non-plan-shaped synthesis still presents (the selector raises over the OLD
// pin, which is still valid) but does not replace it.
type PresentOutcome struct{ Pinned bool }

// ApproveOutcome carries the selected apply contract. Armed=false means no
// contract existed (nothing pinned and no fallback) — the lifecycle returns to
// Idle and nothing must execute.
type ApproveOutcome struct {
	Contract string
	Armed    bool
}

// Controller owns the plan-mode lifecycle. Held as a pointer on Session
// (s.planCtl); the zero value is a ready Idle machine. All fields are
// unexported ON PURPOSE — state moves only through the transition methods
// below, and reads go through the locked accessors. Nil-receiver-safe reads
// let callers skip the `planCtl != nil` dance.
type Controller struct {
	mu    sync.Mutex
	phase Phase
	// epoch identifies one plan session: bumped on Enter and on both exits
	// (Approve/Cancel). Async verdicts computed against one epoch must be
	// re-validated against the current one before they act (the intake gate) —
	// the same state-version rule as the scheduler's expectActive.
	epoch int

	revision      int // times the current plan has been revised (event trail)
	reflectRounds int // extra research rounds the reflection gate triggered

	savedModel string // model to restore when leaving plan mode

	// lastPlan is the pinned apply contract: the most recently PRESENTED
	// plan-shaped synthesis. Preferred over any "last rendered text" at
	// approval because chatter after the plan (a reviewer summary, a follow-up
	// answer) must never become the contract (the wrong-plan handoff bug).
	lastPlan  string
	applyText string // the approved contract, carried into the apply turn's doctrine
	slug      string // saved-plan filename slug: one plan session = one file across revisions
	task      string // the task text this plan session was anchored to at Enter
	yolo      bool   // suppress HITL questions + auto-execute without the selector

	// commitGateResolved is the plan selector's commit-before-work one-shot: armed when
	// the selector's commit choice is answered (ResolveCommitGate), consumed exactly once
	// by the apply turn's gate (ConsumeCommitGate). It lives IN the machine because its
	// lifecycle is plan-shaped — Enter and Cancel clear it structurally, which is the
	// stale-latch class of bug (a prior plan's answered gate silently skipping this
	// plan's dirty-tree check) that used to require remember-to-reset lines in runtime.
	commitGateResolved bool
}

// Opt is a functional option for Enter — the hook for plan-scoped transient
// state. Enter resets everything first, then applies opts, so a stale flag can
// never leak into the next plan session; after unexporting, opts are the ONLY
// write path to task/yolo, which makes reset-before-opts structural.
type Opt func(*Controller)

// WithYolo suppresses human-in-the-loop questions during planning (auto-resolving
// them with the model's recommended choice); the TUI also auto-executes the plan
// without showing the approval selector.
func WithYolo() Opt { return func(c *Controller) { c.yolo = true } }

// WithTask anchors this plan session to the task text that started it — the
// intake gate classifies later submissions against this anchor.
func WithTask(task string) Opt { return func(c *Controller) { c.task = task } }

// Enter starts a plan session: Idle → Researching. Already planning or applying
// → silent no-op (zero Effects): double /plan and enter_plan-while-planning are
// normal user races, and entering during an apply would corrupt the lifecycle.
// currentModel is saved for restore at exit.
func (c *Controller) Enter(currentModel string, opts ...Opt) Effects {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phase != Idle {
		return Effects{}
	}
	c.phase = Researching
	c.epoch++
	c.revision = 0
	c.reflectRounds = 0
	c.lastPlan = "" // a fresh plan session — never carry a prior plan as this one's contract
	c.applyText = ""
	c.slug = "" // fresh plan → a fresh saved-plan file
	c.yolo = false
	c.task = ""
	c.commitGateResolved = false // a fresh plan can't inherit a prior selector's answered gate
	for _, o := range opts {
		o(c)
	}
	c.savedModel = currentModel
	eff := Effects{ClearTodos: true, Emit: events.KindPlanStarted}
	return eff
}

// BeginTurn is the per-turn reset: a presented plan goes back to Researching at
// the top of the next plan-mode turn, so an interrupted turn (Ctrl-C on a
// clarifying question) never leaves a stale "Plan ready" selector armed with
// nothing rendered behind it. No-op in every other phase.
func (c *Controller) BeginTurn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phase == Presented {
		c.phase = Researching
	}
}

// NoteReflect counts a reflection-gate research round; returns the new count
// for the status line. No-op (returning the current count) outside plan mode.
func (c *Controller) NoteReflect() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phase.Planning() {
		c.reflectRounds++
	}
	return c.reflectRounds
}

// Present lands a synthesis: Researching|Presented → Presented. The pin guard
// lives HERE: the contract is replaced only when nothing is pinned yet or the
// synthesis is plan-shaped — a conversational answer to a related follow-up
// must never overwrite the contract /execute will run (the clobber bug). When
// the pin is replaced, Effects.SavePlan carries the text for the durable copy.
func (c *Controller) Present(text string) (Effects, PresentOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	text = strings.TrimSpace(text)
	if !c.phase.Planning() || text == "" {
		return Effects{}, PresentOutcome{}
	}
	c.phase = Presented
	if c.lastPlan != "" && !PlanShaped(text) {
		return Effects{}, PresentOutcome{} // keep the pinned contract; still presentable over it
	}
	c.lastPlan = text
	return Effects{SavePlan: text}, PresentOutcome{Pinned: true}
}

// NotePlanTurn records a proposed/revised plan after a planning turn that
// produced output, advancing the revision counter for the memory trail.
func (c *Controller) NotePlanTurn(hasOutput bool) Effects {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.phase.Planning() || !hasOutput {
		return Effects{}
	}
	kind, payload := events.KindPlanProposed, map[string]any{"revision": 0}
	if c.revision > 0 {
		kind, payload = events.KindPlanRevised, map[string]any{"revision": c.revision}
	}
	c.revision++
	return Effects{Emit: kind, EmitPayload: payload}
}

// Approve is the /execute transition: Researching|Presented → Applying, arming
// the apply contract. Contract selection lives here — the PINNED plan wins;
// lastTextFallback (the session's last rendered text) is used only when nothing
// was ever pinned. No contract at all → Idle with Armed=false (nothing may
// execute). From Idle/Applying → no-op (executePlanTool turns that into a
// model-facing error; re-arming mid-apply is structurally impossible).
func (c *Controller) Approve(lastTextFallback string) (Effects, ApproveOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.phase.Planning() {
		return Effects{}, ApproveOutcome{}
	}
	c.epoch++
	contract := strings.TrimSpace(c.lastPlan)
	if contract == "" {
		contract = strings.TrimSpace(lastTextFallback)
	}
	eff := Effects{Emit: events.KindPlanApproved, EmitPayload: map[string]any{"revisions": c.revision}}
	if c.savedModel != "" {
		eff.SetModel = c.savedModel
		c.savedModel = ""
	}
	if contract == "" {
		c.phase = Idle
		return eff, ApproveOutcome{}
	}
	c.phase = Applying
	c.applyText = contract
	return eff, ApproveOutcome{Contract: contract, Armed: true}
}

// Cancel abandons the plan session: Researching|Presented → Idle. lastPlan and
// slug survive (recall_plan still works; only Enter wipes them). No-op outside
// plan mode — Cancel is called defensively from several TUI paths.
func (c *Controller) Cancel() Effects {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.phase.Planning() {
		return Effects{}
	}
	c.phase = Idle
	c.epoch++
	c.commitGateResolved = false // cancelled plan → drop any armed commit-gate one-shot
	eff := Effects{Emit: events.KindPlanCancelled, EmitPayload: map[string]any{"revisions": c.revision}}
	if c.savedModel != "" {
		eff.SetModel = c.savedModel
		c.savedModel = ""
	}
	return eff
}

// ResolveCommitGate arms the commit-before-work one-shot: the plan selector's
// commit choice was answered, so the apply turn's gate must not re-ask. Armed
// only while a plan is on the table (or already approved — the selector's
// Execute row fires ExitPlan first in some flows).
func (c *Controller) ResolveCommitGate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phase != Idle {
		c.commitGateResolved = true
	}
}

// ConsumeCommitGate consumes the one-shot: true exactly once per arm.
func (c *Controller) ConsumeCommitGate() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	armed := c.commitGateResolved
	c.commitGateResolved = false
	return armed
}

// ApplyDone ends the apply turn: Applying → Idle, contract cleared. It is one
// of exactly two clears of the apply state (with ApplyAborted) — the raw
// tuple-assignment clears that used to live in runtime are gone.
func (c *Controller) ApplyDone() { c.clearApply() }

// ApplyAborted clears an armed-but-never-run contract (commit-gate abort, Esc
// in the approval window): Applying → Idle.
func (c *Controller) ApplyAborted() { c.clearApply() }

func (c *Controller) clearApply() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phase != Applying {
		return
	}
	c.phase = Idle
	c.applyText = ""
}

// RecordSaveSlug records the saved-plan filename slug minted when the plan was
// first written to the plans store — reused across revisions so one plan
// session is one file.
func (c *Controller) RecordSaveSlug(slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slug = slug
}

// --- Read accessors. All nil-receiver-safe (a nil Controller is Idle), all
// locked, so no caller ever needs planCtl != nil or an external mutex.

// Phase returns the current lifecycle phase.
func (c *Controller) Phase() Phase {
	if c == nil {
		return Idle
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.phase
}

// Planning reports plan mode proper (Researching or Presented) — the old Active.
func (c *Controller) Planning() bool { return c.Phase().Planning() }

// IsApplying reports the apply phase — the old Applying boolean.
func (c *Controller) IsApplying() bool { return c.Phase() == Applying }

// InFlow reports any non-Idle phase (planning or applying).
func (c *Controller) InFlow() bool { return c.Phase() != Idle }

// Presentable reports that the most recent plan turn landed an approvable plan
// — the TUI raises the selector on it.
func (c *Controller) Presentable() bool { return c.Phase() == Presented }

// Epoch identifies the current plan session for async-verdict staleness checks.
func (c *Controller) Epoch() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// PhaseEpoch returns phase and epoch atomically — the pair the intake gate
// stamps on a submission so an async verdict can be staleness-checked later.
func (c *Controller) PhaseEpoch() (Phase, int) {
	if c == nil {
		return Idle, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.phase, c.epoch
}

// Snapshot returns the plan anchor task + pinned draft atomically — what the
// intake classifier judges relevance against.
func (c *Controller) Snapshot() (task, draft string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.task, c.lastPlan
}

// ApplyContract returns the armed contract text ("" outside Applying).
func (c *Controller) ApplyContract() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applyText
}

// Revision returns how many times the current plan has been revised.
func (c *Controller) Revision() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revision
}

// ReflectRounds returns the reflection-gate round count for this session.
func (c *Controller) ReflectRounds() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reflectRounds
}

// Yolo reports the auto-resolve/auto-execute flag for this plan session.
func (c *Controller) Yolo() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.yolo
}

// Slug returns this plan session's saved-plan filename slug ("" until first saved).
func (c *Controller) Slug() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slug
}

// --- Plan-shape detection (the Present pin guard's structural signal).

// planStepRe matches a numbered step line ("1. …" / "12) …") at the start of a
// line — the skeleton the synthesis doctrine mandates ("NUMBERED Steps").
var planStepRe = regexp.MustCompile(`(?m)^\s{0,3}\d{1,3}[.)]\s`)

// PlanShaped reports whether text carries a plan's mandated skeleton: at least
// two numbered step lines. A "simplify the plan" revision keeps its numbered
// steps no matter how short it gets; a prose answer to a follow-up has none.
func PlanShaped(text string) bool { return len(planStepRe.FindAllStringIndex(text, 3)) >= 2 }
