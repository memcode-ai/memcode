package runtime

import (
	"bufio"
	"context"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/focus"
	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/mood"
	"github.com/memcode-ai/memcode/internal/agent/room"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/hooks"
	"github.com/memcode-ai/memcode/internal/objectives"
	"github.com/memcode-ai/memcode/internal/scripts"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/skills"
	"github.com/memcode-ai/memcode/internal/structure"
	"github.com/memcode-ai/memcode/internal/wire"
)

// ChatState holds the evolving state of an interactive session — the system
// prompt, the running message history, and the queued follow-ups — so any
// front-end (the line-REPL below or the TUI) can pump input lines via Submit
// while the runtime owns routing and the agent loop.
type ChatState struct {
	sys      promptSpec
	messages []wire.Message
	queued   []input.Bundle
}

// StartChat begins an interactive session: assigns a session id, loads
// remembered approvals, records the start event, and builds the system prompt.
// Pump input with Submit; finish with EndChat. When SetResume was called, the
// saved transcript is re-entered instead of minting a fresh session — the
// episodic log appends to the same session dir, so memory stays one thread.
func (s *Session) StartChat(ctx context.Context) *ChatState {
	var resumedMsgs []wire.Message
	if s.resumeID != "" {
		if msgs, err := loadTranscript(s.root, s.resumeID); err == nil {
			s.setSessionID(s.resumeID)
			resumedMsgs = msgs
		} else {
			s.printf("  could not resume %s: %v — starting fresh\n", s.resumeID, err)
		}
		s.resumeID = ""
	}
	// A caller-pinned id (SetSessionID) wins: use it verbatim so the gateway can
	// resume-or-create a stable per-conversation session. Otherwise mint a fresh id
	// for a brand-new or non-resumed chat — including the case where sessionID is
	// left over from a prior chat on this same Session (resumedMsgs == nil).
	switch {
	case s.pinnedID != "":
		s.setSessionID(s.pinnedID)
	case s.sessionID == "" || resumedMsgs == nil:
		s.setSessionID(newSessionID())
	}
	// Long-lived session scope: background jobs survive turns and die with the
	// session. Cancellable so EndChat can actually END it — the turn-boundary
	// transcript snapshot gates on this ctx, and without a real cancel a turn
	// finishing after teardown would still write into the workspace.
	s.bgCtx, s.bgCancel = context.WithCancel(ctx)
	s.approvals = s.loadApprovals(ctx)
	s.todos = nil // fresh session → fresh scratchpad
	head := gitHead(ctx, s.root)
	s.headSHA = head // provenance stamp for every signal this session emits
	s.emit(ctx, events.KindAgentSessionStarted, map[string]any{
		"mode": string(s.mode), "model": s.model, "head_sha": head, "interactive": true,
		"resumed": resumedMsgs != nil})
	s.startSessionLog(head)                                 // open the append-only episodic log for this session
	s.skills = skills.DiscoverIn(s.root, s.extraSkillRoots) // own skills + Claude Code's installed plugin skills
	s.approvedSkills = loadApprovedSkills(s.root)           // skills the user said "don't ask again" for (persists across sessions)
	s.nudgedSkills = map[string]bool{}                      // per-session: a matched skill is nudged once, not every turn
	if list, err := scripts.List(s.root); err == nil {
		s.scripts = list // saved reusable command sequences (.memcode/scripts)
	}
	s.nudgedScripts = map[string]bool{} // per-session: a matched script is nudged once, not every turn
	s.connectMCP(ctx, true)             // connect .mcp.json servers (interactive: prompts + OAuth allowed)
	s.userMd = s.userInstructions(ctx)  // MEMCODE.md (or CLAUDE.md) — standing instructions, injected every turn (see runTurn)
	s.memoryMd = s.userMemory(ctx)      // durable memory (global + project memory.md) — facts, injected every turn
	sys := s.chatSpec(s.repoOverview(ctx))
	// Skills are NOT dumped into context — the prompt only POINTS at the skill dirs (a blurb
	// for every installed skill, ≈100+ with host plugins, would be wasted context == money).
	// The model greps/reads them on demand and loads one with the skill tool. See skills.Roots.
	if ptr := skillsPointer(skills.RootsIn(s.root, s.extraSkillRoots)); ptr != "" {
		sys = sys.withExtra(ptr)
	}
	if ptr := scriptsPointer(s.scripts); ptr != "" {
		sys = sys.withExtra(ptr)
	}
	if kp := knowledgePointer(s.root); kp != "" { // name the built-in knowledge packs (lead with detected stacks)
		sys = sys.withExtra(kp)
	}
	if o := s.priorSessionOrientation(ctx); o != "" {
		sys = sys.withExtra(o)
	}
	// Promote/demote standing preferences based on accumulated evidence. Silent:
	// the threshold (≥3 signals, ≥2 sessions, weighted score ≥ 2.0) is the gate.
	// Bounded: Reduce scans ≤5000 preference_signal events (idx_events_kind) and
	// materializes in a single tx. The user reviews via /preferences or by
	// editing .memcode/prefs/. No s.ask — safe in StartChat (see app.go:272).
	s.applyPreferencePromotions(ctx)
	// Inject confirmed standing preferences into the system prompt (top 5 by
	// weight, ≤10 lines). The model sees these as standing context.
	prefBlock, prefIDs := s.inlinePrefsTop(ctx)
	if prefBlock != "" {
		sys = sys.withExtra(prefBlock)
	}
	// Same rigor for distilled failure lessons: promote what recurred across
	// sessions, then surface the top lessons as background DATA (never
	// instructions — the poisoning boundary). See internal/lessons.
	s.applyLessonPromotions(ctx)
	lessonBlock, lessonIDs := s.inlineLessonsTop()
	if lessonBlock != "" {
		sys = sys.withExtra(lessonBlock)
	}
	// Record WHICH rules rode this session's prompt — the adherence judge may
	// only score rules the model actually saw. Dual-write like every signal:
	// SQLite event (queried by processOutcomes) + canonical session-log record.
	if len(prefIDs) > 0 || len(lessonIDs) > 0 {
		s.emit(ctx, events.KindContextInlined, map[string]any{
			"session_id": s.sessionID, "lesson_ids": lessonIDs, "pref_ids": prefIDs,
		})
		s.slog.Append(sessionlog.Record{
			Kind: sessionlog.KindContextInlined, LessonIDs: lessonIDs, PrefIDs: prefIDs,
		})
	}
	// Post-session learning loop: distill lessons from finished sessions whose
	// git fate is now known (corrected/rejected), and judge rule adherence.
	// Async — startup never waits on it — but joinable: EndChat cancels and
	// waits, so its writes under the repo root never race whatever tears the
	// workspace down next.
	octx, ocancel := context.WithCancel(s.bgCtx)
	s.outcomeCancel = ocancel
	done := make(chan struct{})
	s.outcomeDone = done
	go func() { defer close(done); s.processOutcomes(octx) }()
	// User session_start hooks: whatever they print becomes standing context
	// (env facts, sprint notes — deterministic injection the user owns).
	if out := s.runSessionHooks(ctx, hooks.SessionStart); out != "" {
		sys = sys.withExtra("Session-start hook context (user-configured):\n" + out)
	}
	st := &ChatState{sys: sys, messages: resumedMsgs}
	s.liveChat = st // wire so SetPin/SetVendor can strip thinking blocks on a model switch
	if resumedMsgs != nil {
		s.printf("  ↩ resumed %s (%d messages)\n", s.sessionID, len(resumedMsgs))
	}
	return st
}

// priorSessionOrientation is the cold-start FOCUS digest — where the user's head is,
// derived from the current WORK BURST of prior sessions (focus.FromLog) plus durable
// objectives. It's auto-injected into the system prompt so the agent orients WITHOUT
// being asked — the numbered OPEN THREADS a status answer must account for. Empty
// when there's nothing to surface. Orientation, not truth — the agent still verifies
// against git/files. See package focus (the cognitive axis; room is the emotional one).
func (s *Session) priorSessionOrientation(ctx context.Context) string {
	st := s.focusNow(ctx)
	if st.Empty() {
		return ""
	}
	return s.redactor.Redact(focus.Render(st))
}

// focusNow computes the FocusState for the LIVE work arc — plus durable
// objectives. Thin wrapper over focus.FromLog, the ONE window+reducer shared
// with the memcode{session} tool, so cold-start orientation (here) and
// /predict grounding can never drift from what the tool reports. (Note:
// /overview does NOT consume this — it reads objectives directly.)
func (s *Session) focusNow(ctx context.Context) focus.State {
	var objs []objectives.Objective
	if cur, err := objectives.New(s.store).Current(ctx); err == nil {
		objs = cur
	}
	return focus.FromLog(s.root, s.sessionID, objs)
}

// Submit routes one raw input line and runs the resulting turn(s) to
// completion, draining any queued follow-ups. Output streams to the session
// writer; approvals go through the session approver. Blank lines are ignored.
func (s *Session) Submit(ctx context.Context, st *ChatState, line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	dec := input.Parse(line, s.root)
	// Caller-resolved media (gateway channel attachments) ride this turn's bundle
	// through the normal attachment path, then clear — next turns carry nothing.
	if len(s.taskAttachments) > 0 {
		dec.Bundle.Attachments = append(dec.Bundle.Attachments, s.taskAttachments...)
		s.taskAttachments = nil
	}
	// Did the user explicitly authorize changing tests/specs/behavior this turn? If so,
	// editing tests is the WORK; if not, weakening a test is gated as a self-heal cheat.
	s.testEditIntent = userIntendsTestChange(dec.Bundle.Text)
	s.lastUserText = dec.Bundle.Text // ground truth for the authorization judge

	// `$` direct-shell lane: run the command verbatim and stop. NOT an agent turn —
	// no model, no mood/room scoring, no objective steering. The user typed a command;
	// we run it (gated) and show its output.
	if dec.Route == input.Shell {
		s.runShell(ctx, dec.Bundle.Text)
		return
	}

	// Read the interaction friction of this turn (deterministic, local). An
	// explicit interrupt is itself a friction signal, so nudge it up.
	reading := s.mood.Observe(mood.Score(dec.Bundle.Text), dec.Bundle.Text)
	if dec.Route == input.Interrupt {
		reading = s.mood.Bump(0.2, "interrupt")
	}
	// Message cadence: a quick corrective follow-up ("no, that's wrong" seconds
	// after the agent acted) is a strong "off track" cue → raise friction.
	corrective := dec.Route == input.Interrupt || reading.Frustration >= 0.4
	cad := s.cadence.Observe(dec.Bundle.Text, corrective)
	if cad.RapidCorrection {
		reading = s.mood.Bump(0.2, "rapid-correction")
	}

	s.emit(ctx, events.KindUserInputReceived, map[string]any{
		"route": string(dec.Route), "reason": dec.Reason, "attachments": len(dec.Bundle.Attachments),
		"mood": reading, "cadence": cad})

	// Reduce the event log + live reading into the room state, then act on it:
	// the assessed mode drives the permission policy and the per-turn guidance.
	s.room = room.Assess(reading, room.Gather(ctx, s.store, s.sessionID)) // current session only — no cross-session friction leak
	s.recordMood(ctx, dec.Bundle.Text, reading)
	// Thinking + tier are an LLM judgment (turn_intent, fired concurrently and
	// joined in runLoop); the deterministic pieces here are FACTS only — the
	// /effort override pins, room state sets the provisional effort.
	if s.provisionalEffort() {
		s.startTurnJudge(ctx, dec.Bundle.Text)
	}
	s.turnHighRisk = highRiskTurn(dec.Bundle.Text) // high-blast-radius surface → escalate the backend

	s.printf("  ↳ %s — %s\n", dec.Route, dec.Reason)
	if s.observer != nil {
		s.observer.Routed(dec.Route, dec.Reason)
		s.observer.Mood(reading)
		s.observer.Room(s.room)
	}

	run := func(explicitSteer bool, b input.Bundle) {
		// The SAME decision core as the TUI (decideIntake) — the plan-intake gate lives
		// there, not here. Headless is SERIAL: no active tx, no coalescing clock, so the
		// only reachable actions are AwaitVerdict (classify synchronously — this path
		// already blocks the caller), DeferForPlan (park it), and StartTurn. Only an
		// EXPLICIT `+steer` bypasses the gate — input.Parse's DEFAULT route is Steer, so
		// the parsed route can't stand in for user intent here.
		gateRoute := input.Queue
		if explicitSteer {
			gateRoute = input.Steer
		}
		verdict, verdictEpoch, title := VerdictNone, 0, ""
		for {
			phase, epoch := s.PlanPhaseEpoch()
			gate := GateInput{Phase: phase, PlanEpoch: verdictEpoch, CurrentEpoch: epoch, Verdict: verdict}
			act := decideIntake(gateRoute, gate, false, 0, time.Time{}, time.Time{})
			if act == ActAwaitVerdict {
				task, draft := s.PlanGateSnapshot()
				related, t := s.ClassifyPlanMessage(ctx, b.Text, task, draft)
				verdict, verdictEpoch, title = VerdictSeparate, epoch, t
				if related {
					verdict = VerdictRelated
				}
				continue
			}
			if act == ActDeferForPlan {
				s.DeferWhilePlanning(b.Text, title)
				s.printf("◇ separate — queued to run after the plan\n")
				return
			}
			break // ActStartTurn — headless never has an active tx here
		}
		if s.observer != nil {
			s.observer.Busy(true)
			defer s.observer.Busy(false)
		}
		s.runTurn(ctx, st, b)
		// If that turn just left plan mode (the model called cancel_plan/execute_plan), replay
		// whatever the intake gate parked as SEPARATE while planning. st.queued is this
		// frontend's one drain sink (PlanDrainSink's QueueBehind and StartNow both collapse
		// to FIFO here — there's no scheduler; see vxui's drainPlanDeferred for the TUI).
		if !s.Planning() {
			for _, text := range s.DrainPlanDeferred() {
				st.queued = append(st.queued, input.Bundle{Text: text})
			}
		}
	}

	switch dec.Route {
	case input.Interrupt:
		st.queued = nil
		s.emit(ctx, events.KindInputInterrupted, nil)
		s.printf("  cleared %s pending.\n", "all")
		s.notifyQueue(st)
		if strings.TrimSpace(dec.Bundle.Text) != "" || len(dec.Bundle.Attachments) > 0 {
			run(false, dec.Bundle) // run the corrective instruction now
		}
	case input.Queue:
		s.emit(ctx, events.KindInputQueued, nil)
		st.queued = append(st.queued, dec.Bundle)
		s.printf("  queued (%d pending).\n", len(st.queued))
		s.notifyQueue(st)
		return
	default: // steer / coalesce
		s.emit(ctx, events.KindInputSteered, nil)
		run(strings.HasPrefix(strings.TrimSpace(line), "+"), dec.Bundle)
	}

	for len(st.queued) > 0 {
		next := st.queued[0]
		st.queued = st.queued[1:]
		s.notifyQueue(st)
		s.printf("\n— queued: %s\n", next.Text)
		run(false, next)
	}
}

// runTurn executes one user turn: compact at the safe boundary, append the user
// message, log it, and run the agent loop (plan-aware). Shared by the line-REPL Submit
// and the TUI transaction executor (RunTransaction).
func (s *Session) runTurn(ctx context.Context, st *ChatState, b input.Bundle) {
	// Turn boundary = resume boundary: snapshot the history when this turn ends
	// (idempotent on the recursive apply chain — the last write wins with the same
	// content). Gate on the SESSION context, not the turn ctx: under the scheduler
	// every turn has its own cancellable ctx, so an Esc (turn cancel) must STILL
	// snapshot — the partial turn is real history that --continue would otherwise
	// lose. Only a real session teardown (bgCtx done) skips the write.
	defer func() {
		if s.bgCtx.Err() == nil {
			s.persistTranscript(st)
		}
	}()
	s.lastErr = nil // fresh turn; set below if the loop returns a terminal error
	// Rewind point for this turn's edits (writes nothing unless a file is edited).
	s.curCkpt = s.ckpt.Begin(b.Text)
	// APPLY an approved plan: this is the /execute handoff. Run it as a clean, contract-
	// bound turn — drop the planning RESEARCH transcript (the plan is the contract, carried
	// in the apply doctrine, not the raw greps that produced it, which only tempt re-research
	// and balloon context). Execute on the SESSION model (Sonnet, the cheap default): the
	// expensive reasoning already happened ONCE when Opus wrote the plan — implementing a
	// clear plan (edit, build, test, fix) is exactly what the cheaper coder model is for, and
	// the CONTRACT (apply doctrine + clean context), not raw model strength, is what keeps it
	// on-plan. Opus owns the plan; Sonnet owns execution; Opus returns only on exceptions —
	// runLoop's self-repair path already escalates to Opus when execution gets STUCK (breaks
	// an edit), and the doctrine tells the model to STOP and ask if the plan itself is wrong.
	// Effort is OFF (no thinking): Opus already did the hard reasoning when it wrote the
	// plan, and Sonnet still reasons in its response without a thinking budget. Executing a
	// clear plan (edit/build/test/fix) is mechanical; the self-repair path escalates to
	// Opus+high only if a step actually gets stuck (breaks an edit). No thinking tax on the
	// cheap handoff.
	if s.planCtl.IsApplying() {
		// A large work block is about to start (an approved plan). If the tree has
		// uncommitted changes, offer to commit first so the plan's edits land on a
		// clean base and can be reviewed/reverted on their own — the user may have
		// unrelated in-flight work they don't want tangled with the agent's. Aborts
		// the apply if they choose to commit first; remembered choice skips it.
		if !s.commitGateOK(ctx) {
			s.planCtl.ApplyAborted() // armed-but-never-run contract cleared via the machine
			return
		}
		// ApplyDone is DEFERRED so no error path can strand the machine in Applying
		// (a stuck Applying phase would make the post-loop chain below re-fire forever).
		defer s.planCtl.ApplyDone()
		st.messages = st.messages[:0]
		st.messages = append(st.messages, wire.Message{Role: "user", Blocks: s.userBlocks(ctx, b)})
		s.logUser(b)
		s.setTurnEffort(wire.EffortOff) // Opus already thought; execution is mechanical (escalates if stuck)
		sys := s.applySpec(s.planCtl.ApplyContract())
		_, _, err := s.runLoop(ctx, s.roomSpec(sys), &st.messages)
		if err != nil {
			s.lastErr = err
			s.printf("  error: %v\n", err)
		}
		return
	}
	// A finished checklist clears once the user moves on. If every prior todo is done or
	// skipped, this fresh turn is a new unit of work — drop the completed list so it doesn't
	// linger (the live panel clears via the observer). A list with ANY pending item is left
	// intact (the task is still in flight). New-chapter commands (/plan, /yolo, /clear) clear
	// it up front separately (EnterPlan / clearSession).
	if len(s.todos) > 0 && pendingTodos(s.todos) == 0 {
		s.todos = nil
		if s.observer != nil {
			s.observer.Todos(nil)
		}
	}
	// Review any project-scoped MCP servers held back at connect time: prompt to approve, then
	// connect the approved ones for the rest of the session. Runs at most once (clears pending).
	s.reviewPendingMCP(ctx)
	// Safe boundary: shrink the history BEFORE assembling this turn, so a smaller prompt
	// is what gets routed (keeping the turn on the cheap lane instead of spilling on
	// context_over_lanes). No-op until the session is long.
	s.compactIfNeeded(ctx, st)
	st.messages = append(st.messages, wire.Message{Role: "user", Blocks: s.userBlocks(ctx, b)})
	s.logUser(b) // episodic memory: what the user actually asked, in order
	// In plan mode the system prompt and tools are research-only; otherwise the room's
	// mode shapes strategy this turn (repair/explain/explore/…).
	base := st.sys
	if s.planCtl.Planning() {
		base = s.planSpec(s.repoOverview(ctx))
	}
	// Refresh the personality fact per turn. The chat spec is built once at StartChat and
	// cached, so without this a mid-session /personality change never takes effect and
	// "random" freezes to one voice. withFact copies the facts map (cached spec untouched);
	// an empty value is fine — the gateway treats "" as "no voice".
	base = base.withFact("personality", s.personalityFact())
	if s.extraMile { // refresh per-turn too (cached spec) so a mid-session /extramile toggle lands
		base = base.withFact("extramile", "on")
	}
	if s.userMd != "" { // user's MEMCODE.md rides every turn (chat + plan), as standing doctrine
		base = base.withExtra(s.userMd)
	}
	if s.memoryMd != "" { // durable memory (global + project) rides every turn as background facts
		base = base.withExtra(s.memoryMd)
	}
	if blk := supplementalBlock(s.supplemental); blk != "" { // caller-supplied context (agent runtime); empty for CLI
		base = base.withExtra(blk)
	}
	if nudge := s.skillNudge(b.Text); nudge != "" { // request names an installed skill → point right at it
		base = base.withExtra(nudge)
	}
	if nudge := s.scriptNudge(b.Text); nudge != "" { // request names a saved script → point right at it
		base = base.withExtra(nudge)
	}
	if _, _, err := s.runLoop(ctx, s.roomSpec(base), &st.messages); err != nil {
		s.lastErr = err
		s.printf("  error: %v\n", err)
	}
	// The model called execute_plan this turn (the user told it to go): the state machine is now in
	// the apply phase — run it immediately, chaining into runTurn's Applying branch above, so a
	// "execute" reply actually EXECUTES rather than ending on a re-proposed plan.
	if s.planCtl.IsApplying() {
		s.runTurn(ctx, st, input.Bundle{Text: applyExecuteInstruction})
		return
	}
	if s.planCtl.Planning() {
		s.notePlanTurn(ctx) // record plan_proposed / plan_revised
	}
}

// applyExecuteInstruction is the user-message the chained apply turn runs on after execute_plan —
// the plan itself is the contract (carried in the apply doctrine via planCtl.ApplyText).
const applyExecuteInstruction = "Begin implementing the approved plan now, working its steps in order."

// scoreTurn reads the interaction friction for one turn's text and sets the per-turn
// policy (room mode, thinking effort, model, high-risk). Route-agnostic — the scheduler
// already decided queue/steer/start — so it's the scorer for transaction execution.
func (s *Session) scoreTurn(ctx context.Context, text string) {
	s.testEditIntent = userIntendsTestChange(text)
	s.lastUserText = text
	reading := s.mood.Observe(mood.Score(text), text)
	cad := s.cadence.Observe(text, reading.Frustration >= 0.4)
	if cad.RapidCorrection {
		reading = s.mood.Bump(0.2, "rapid-correction")
	}
	s.emit(ctx, events.KindUserInputReceived, map[string]any{"mood": reading, "cadence": cad})
	s.room = room.Assess(reading, room.Gather(ctx, s.store, s.sessionID))
	s.recordMood(ctx, text, reading)
	// Thinking + tier are an LLM judgment (turn_intent, fired concurrently and
	// joined in runLoop before the first model call); the deterministic pieces
	// here are FACTS only — the /effort override pins, room state sets the
	// provisional effort while the judge is in flight.
	if s.provisionalEffort() {
		s.startTurnJudge(ctx, text)
	}
	s.turnHighRisk = highRiskTurn(text)
	if s.observer != nil {
		s.observer.Mood(reading)
		s.observer.Room(s.room)
	}
}

// RunTransaction executes one scheduled transaction as a turn — the TUI executor's entry
// point. The scheduler already routed (this is an active, promoted transaction), so this
// just scores and runs. A `$`-prefixed transaction takes the direct-shell lane. Steers
// submitted into this transaction while it runs are folded in by runLoop (SetSteerDrain).
func (s *Session) RunTransaction(ctx context.Context, st *ChatState, tx *Transaction) {
	if s.observer != nil {
		s.observer.Busy(true)
		defer s.observer.Busy(false)
	}
	dec := input.Parse(tx.Text, s.root) // for attachments + the `$` shell lane; routing is the scheduler's
	if dec.Route == input.Shell {
		s.runShell(ctx, dec.Bundle.Text)
		return
	}
	s.scoreTurn(ctx, dec.Bundle.Text)
	s.runTurn(ctx, st, dec.Bundle)
}

// RunShellLine runs a `$`/`>` shell-lane line IMMEDIATELY, outside the turn scheduler — it's a
// LOCAL capture action (a command the user typed by hand), not an agent turn, so it must never
// queue behind an active turn. Reports whether the line was actually a shell route (false → the
// caller routes it normally through the scheduler). Safe to call concurrently with a running turn:
// the front-end owns its own busy/spinner state (the observer's Busy is a no-op) and command output
// is marshalled onto the UI thread, so a hand-run `$ git status` interleaves cleanly mid-turn.
func (s *Session) RunShellLine(ctx context.Context, line string) bool {
	dec := input.Parse(line, s.root)
	if dec.Route != input.Shell {
		return false
	}
	s.runShell(ctx, dec.Bundle.Text)
	return true
}

// EndChat records the session-finished event and closes the episodic log.
// Idempotent enough for one call.
func (s *Session) EndChat(ctx context.Context) {
	if s.bgCancel != nil {
		s.bgCancel() // end the session scope: no further turn-boundary writes
		s.bgCancel = nil
	}
	if s.outcomeCancel != nil {
		s.outcomeCancel() // in-flight LLM judge calls abort on the cancel
		select {
		case <-s.outcomeDone:
		case <-time.After(3 * time.Second): // bounded: never let learning stall a quit
		}
		s.outcomeCancel = nil
		s.outcomeDone = nil
	}
	s.runSessionHooks(ctx, hooks.SessionEnd) // fire-and-forget (bounded by the per-hook timeout)
	s.KillAllJobs()                          // reap background jobs so nothing (dev servers, watchers) orphans
	s.closeMCP()                             // tear down MCP server connections (subprocesses / HTTP sessions)
	s.lspMgr.Close()                         // shut down resident language servers (nil-safe)
	s.emit(ctx, events.KindAgentSessionFinished, map[string]any{"interactive": true})
	s.endSessionLog()
}

// recordMood persists a high-friction or strong-memory-weight turn as a
// frustration event. The value is NOT "the user was angry" — it's the user's
// DIRECTION captured with its intensity, so a forceful instruction ("do NOT add
// a paid vendor") is recalled and weighted as stronger doctrine later.
func (s *Session) recordMood(ctx context.Context, text string, r mood.Reading) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if r.Frustration < 0.6 && s.room.Policy.MemoryWeight != "strong" {
		return
	}
	s.emit(ctx, events.KindFrustration, map[string]any{
		"text": text, "frustration": r.Frustration, "intensity": r.Intensity,
		"state": string(r.State), "signals": r.Signals, "weight": s.room.Policy.MemoryWeight,
	})
}

// notifyQueue pushes the current queue contents to the observer, if any.
func (s *Session) notifyQueue(st *ChatState) {
	if s.observer == nil {
		return
	}
	items := make([]string, len(st.queued))
	for i, b := range st.queued {
		items[i] = b.Text
	}
	s.observer.QueueChanged(items)
}

// Chat runs the line-REPL interactive session: streamed stdin with time-based
// coalescing and routing, multimodal attachments, persisted events. The TUI
// drives the same StartChat/Submit/EndChat seams without owning stdin.
func (s *Session) Chat(ctx context.Context) error {
	st := s.StartChat(ctx)
	s.printf("memcode · interactive (%s mode) · %s\n", s.mode, s.model)
	s.printf("Type a request; drag files/screenshots in. `> ` queues, `! ` interrupts. Ctrl-D to exit.\n\n")

	lines := streamLines(ctx)
	for {
		s.printf("\n› ")
		line, ok := coalesce(lines, coalesceWindow)
		if !ok {
			break // EOF
		}
		s.Submit(ctx, st, line)
	}
	s.EndChat(ctx)
	s.printf("\nbye.\n")
	return nil
}

func (s *Session) repoOverview(ctx context.Context) string {
	topo, err := structure.Load(ctx, s.store)
	if err != nil || len(topo.Subsystems) == 0 {
		return "(no repo model yet — run `memcode init`)"
	}
	names := make([]string, 0, len(topo.Subsystems))
	for _, sub := range topo.Subsystems {
		names = append(names, sub.Key)
	}
	return "Subsystems: " + strings.Join(names, ", ")
}

// streamLines reads stdin lines into a channel so the loop can coalesce by time.
func streamLines(ctx context.Context) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			select {
			case ch <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// coalesce returns the first line, merged with any further lines that arrive
// within window of each other (rapid typing = one thought).
func coalesce(ch <-chan string, window time.Duration) (string, bool) {
	first, ok := <-ch
	if !ok {
		return "", false
	}
	parts := []string{first}
	timer := time.NewTimer(window)
	defer timer.Stop()
	for {
		select {
		case l, ok := <-ch:
			if !ok {
				return strings.Join(parts, " "), true
			}
			parts = append(parts, l)
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(window)
		case <-timer.C:
			return strings.Join(parts, " "), true
		}
	}
}
