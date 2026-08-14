// Package vxui is memcode's interactive renderer, built on vaxis's Elm-style `ui`
// framework (StatefulWidget/State/Build/HandleEvent). It replaces the Bubble Tea fork:
// the live region (status · composer · footer, plus menus/prompts) is a widget tree that
// the framework lays out and paints every frame, and durable output goes to native
// scrollback via EventContext.AppendString. The framework owns the cursor/region
// bookkeeping, so the inline-render glitch class (and the ShowCursor scrollback-erase bug)
// cannot occur. Engine output and HITL prompts arrive on other goroutines and are
// marshalled onto the UI thread with Runtime().Dispatch (goroutine-safe via PostEvent).
package vxui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/memcode-ai/memcode/catalog"
	vaxis "github.com/memcode-ai/memcode/internal/forks/vaxis"
	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/input"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/checkpoint"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/provider"
	memsync "github.com/memcode-ai/memcode/internal/sync"
	"github.com/memcode-ai/memcode/internal/theme"
	"github.com/memcode-ai/memcode/internal/todos"
)

const (
	composerPrompt      = "→ "
	composerPlaceholder = "Ask memcode…   ·   $ = shell"
)

// Run starts the interactive renderer and blocks until the user quits.
func Run(ctx context.Context, sess *runtime.Session, themeName string) error {
	theme.Set(themeName)
	// Banner BEFORE the inline TUI starts; vaxis's primary screen preserves prior scrollback,
	// so it settles above the live region. Matrix theme plays the original digital-rain intro
	// (skippable by typing — the keystrokes carry into the composer); others print statically.
	matrix := theme.Active().Name == "matrix"
	// Building the banner can be slow (it resolves the serving model + repo orientation), and that
	// whole window runs before vaxis owns the tty. In cooked mode a user who starts typing has their
	// keystrokes echoed as raw garbage and then swallowed when vaxis flips to raw. So render the
	// banner under raw mode (no echo) and CAPTURE whatever was typed, to replay into the composer as
	// the user's first input. Matrix needs none of this: it animates in the live region with vaxis
	// already up, so early keystrokes already flow to the composer.
	var early string
	if !matrix {
		early = captureStartupInput(os.Stdin.Fd(), func(raw bool) {
			printBanner(ctx, sess, raw) // static settled matrix glyph banner to scrollback
		})
	}
	// matrix: the intro animates IN the live region (handled by the app), not a pre-printed splash.
	root := &appWidget{ctx: ctx, sess: sess, themeName: themeName, live: true, introMatrix: matrix, initialInput: early}
	return ui.Run(root, ui.WithDynamicPrimaryScreen(), ui.WithTheme(uiTheme(theme.Active().Palette)))
}

// appWidget is the root StatefulWidget. It carries the immutable wiring the state needs.
type appWidget struct {
	ctx          context.Context
	sess         *runtime.Session
	themeName    string
	live         bool   // true under the real runner; false in headless tests (skips background I/O goroutines)
	introMatrix  bool   // play the matrix digital-rain intro in the live region
	initialInput string // keystrokes captured during the startup banner, replayed into the composer
}

func (w *appWidget) CreateState() ui.State {
	s := &appState{w: w}
	if w.initialInput != "" {
		s.composer = w.initialInput
		s.cursor = len([]rune(w.initialInput))
	}
	return s
}

// appState holds all mutable UI state and the engine wiring. Every field is read/written
// only on the UI thread (Build / HandleEvent / dispatched closures), so no locks are needed.
type appState struct {
	ui.StateBase
	w *appWidget

	rt    ui.Runtime      // marshals background work onto the UI thread
	ectx  ui.EventContext // captured for scrollback appends from dispatched closures
	sty   uiStyles        // theme-derived styles (rebuilt on /theme)
	wired bool

	sched        *runtime.Scheduler
	classifyKick chan struct{} // nudges the background follow-up classifier on a mid-turn queue submit
	chat         *runtime.ChatState

	// defaultVendor caches the deployment's default strong vendor from the
	// /v1/models fetch (resolveServingDefault); "" until fetched. Read by the
	// /status line so the hidden-default vendor isn't hardcoded.
	defaultVendor string

	composer  string
	cursor    int               // rune offset of the insertion point in composer
	slashSel  int               // highlighted row in the slash menu
	escArmed  bool              // a bare Esc was just pressed with text in the composer; a second Esc clears it
	busyOwner runtime.BusyOwner // who owns the busy surface — ONE writer (setBusy); busy() derives

	// Bracketed paste: the terminal frames a paste with PasteStart/PasteEnd and tags every
	// key between them EventPaste. We BUFFER that burst (so embedded newlines don't each
	// submit, and the whole thing isn't split into N commands) and, on PasteEnd, drop a
	// compact [pasted …] token into the composer — the real content is stashed in pastes and
	// expanded on submit. Without this a multi-line paste fired one turn per line.
	pasting    bool
	pasteBuf   strings.Builder
	pasteBytes int               // TRUE size of the in-flight paste (pasteBuf is capped at maxPasteBytes)
	pastes     map[string]string // [pasted …]/[Image #n] token → real content, expanded at submit
	pasteSeq   int
	imageSeq   int // a dragged-in image path collapses to an [Image #n] chip (see commitPaste)

	// theme picker overlay (modal): ↑↓ live-preview, Enter apply, Esc restore.
	themePicking bool
	themeSel     int
	themeOrig    string
	themeNames   []string

	// vendor picker overlay (modal): ↑↓ choose, Enter apply + persist, Esc cancel.
	// Opened by /model (no arg). Lists only the vendors the gateway reports as
	// available (keys present); selecting one calls sess.SetVendor + persists.
	modelPicking bool
	modelSel     int
	modelOrig    string       // the pin at picker-open time ("" = Automatic)
	modelEntries []modelEntry // picker rows: Automatic + the gateway-reported pinnable models
	modelTyping  bool         // endpoint mode: the free-text id-entry stage is active
	modelInput   []rune       // the id being typed in that stage

	// personality picker overlay (modal): ↑↓ choose, Enter apply, Esc cancel.
	personalityChoosing bool
	personalitySel      int
	personalityOrig     string

	// Startup sign-in card (modal): shown at boot when NOT logged in (a local
	// decision — token file presence + the memcode_ prefix; zero network).
	// Enter opens the browser login, Esc dismisses to the signed-out shell
	// (local commands + /login still work).
	loginPrompting bool

	// /apikeys picker overlay (modal, three-stage): provider list ↑↓/Enter →
	// MASKED key entry (never the composer — it echoes to scrollback) →
	// submit; `d` on a set row → confirm delete; `v` → validate. The typed/
	// pasted key lives ONLY in apikeysInput and is zeroed after submit.
	apikeysPicking    bool
	apikeysSel        int
	apikeysRows       []apikeyRow
	apikeysEntering   bool // masked key-entry stage (for apikeysRows[apikeysSel])
	apikeysInput      []rune
	apikeysConfirmDel bool // confirm-delete stage

	extraMileChoosing bool // the /extramile on/off selector is open
	extraMileSel      int

	// rewind picker overlay (modal, two-stage): list ↑↓ choose → Enter review →
	// confirm Enter restore / Esc back. Destructive, so it always confirms.
	rewindChoosing bool
	rewindConfirm  bool // false = list stage, true = confirm stage
	rewindSel      int
	rewindPoints   []checkpoint.Manifest // snapshot at open, NEWEST first

	// sync picker overlay (modal, multi-select): ↑↓ move, Space toggles, a toggles all,
	// Enter persists + syncs, Esc cancels. Unlike the other pickers (single-select radio),
	// this one is checkboxes — Space is the toggle key (Enter commits).
	syncChoosing bool
	syncSel      int
	syncToggles  []bool                   // one per config.SyncTargetAll entry
	syncDetected []memsync.DetectedTarget // existence/managed status from disk

	// plan mode: a plan turn ran and is awaiting Execute / advisor / revise / Cancel.
	planChoosing  bool
	planChoice    int    // highlighted option in the plan selector
	planStage     int    // 0 = fresh (Execute · Ask an advisor · Cancel), 1 = advised (gpt-5.5 weighed in)
	planAdvice    string // stashed advisor prose, folded into the plan by "Revise plan with advice"
	planCommitAsk bool   // dirty tree at selector-raise time → Execute splits into commit-first/without variants
	turnStart     time.Time
	// HITL wait accounting: the "Thinking…" clock must measure LLM/processing time, NOT how long
	// the user took to answer an ask_user / approval card. hitlWaitAt marks when the current card
	// went up (zero = not waiting); hitlWait accumulates finished waits. turnElapsed subtracts both.
	hitlWait    time.Duration
	hitlWaitAt  time.Time
	turnIn0     int // session token totals at turn start; the spinner shows the per-turn delta
	turnOut0    int
	spin        int
	queued      []string
	asyncCancel context.CancelFunc // cancels the in-flight runAsync op (Ctrl+C during /compact//models/advisor)

	// footer cockpit (refreshed off the render path)
	branch     string
	gstat      runtime.GitStat
	agents     int               // live count of running dispatched sub-agents (footer "N agents")
	seenAgents map[string]string // job id → last status (detects running→done transitions for notifications)

	// HITL prompt queue (see hitl.go): hitlQueue[0] is the card on screen (its fields are in
	// pending/askReq below), [1:] wait their turn. One card shows at a time; concurrent asks (the
	// turn + a `$` shell command) queue instead of overwriting each other, and each waiter can
	// withdraw on ctx-cancel. hitlSeq mints waiter ids on the engine side.
	hitlQueue []hitlWaiter
	hitlSeq   int64

	// inline approval prompt
	pending       *runtime.ApprovalRequest
	approval      chan runtime.ApprovalDecision
	approveChoice int // index into approvalOptions() (0 is always Yes/Execute)

	// inline clarifying question (ask_user)
	askReq    *runtime.AskRequest
	askReply  chan runtime.AskResponse
	askChoice int

	todos todos.List // the agent's live work-tracker, rendered as a panel below the status bar

	appendBuf   strings.Builder // line-buffers engine output (scrollback wants whole lines)
	inCodeFence bool            // stream state: inside a ``` block
	lastBlank   bool            // stream state: scrollback currently ends blank
	lastTool    bool            // stream state: last non-blank scrollback line was tool-block content

	// matrix intro (rendered in the live region so the framework handles width/clip/position)
	intro       bool
	introFrame  int
	introRecall string
	width       int
}

// InitState wires the engine once the element is mounted: it captures the runtime + event
// context for async marshalling, routes engine output to scrollback, installs the HITL
// approver, and starts the scheduler + chat.
func (s *appState) InitState() {
	s.sty = makeStyles(theme.Active().Palette)
	ctx := s.Context()
	s.rt = ctx.Runtime()
	s.ectx = ctx.EventContext()

	// Engine output → scrollback. Marshalled onto the UI thread; line-buffered because vaxis
	// scrollback only commits whole, newline-terminated lines.
	s.w.sess.SetOutput(writerFunc(func(b []byte) (int, error) {
		text := string(b)
		s.rt.Dispatch(func() { s.appendChunk(text) })
		return len(b), nil
	}))
	// Observe the live work-tracker: the engine pushes todo updates here (which also stops it
	// dumping the full checklist into scrollback), and they render as a panel below the status bar.
	s.w.sess.SetObserver(vxObserver{s: s})
	// Inline clarifying question: show the card in the live region, block the engine on a reply
	// chan. An empty answer (Esc / interrupt) = the agent proceeds on best judgment.
	s.w.sess.SetAsker(func(c context.Context, req runtime.AskRequest) runtime.AskResponse {
		reply := make(chan runtime.AskResponse, 1)
		r := req
		id := s.nextHitlID()
		// present installs this question as the active card when it reaches the front of the queue.
		present := func() {
			s.askReq = &r
			s.askReply = reply
			s.askChoice = 0
		}
		s.rt.Dispatch(func() { s.SetState(func() { s.enqueueHitl(hitlWaiter{id: id, present: present}) }) })
		select {
		case a := <-reply:
			return a
		case <-c.Done():
			// Gave up (turn cancelled) — withdraw from the queue so it never shows (or stops
			// showing) and the next prompt advances. We don't wait for our turn at the front.
			s.rt.Dispatch(func() { s.SetState(func() { s.withdrawHitl(id) }) })
			return runtime.AskResponse{}
		}
	})
	// Inline approval: show the request in the live region, block the engine on a reply chan.
	s.w.sess.SetApprover(func(c context.Context, req runtime.ApprovalRequest) runtime.ApprovalDecision {
		reply := make(chan runtime.ApprovalDecision, 1)
		r := req
		id := s.nextHitlID()
		present := func() {
			s.pending = &r
			s.approval = reply
			s.approveChoice = 0
		}
		s.rt.Dispatch(func() { s.SetState(func() { s.enqueueHitl(hitlWaiter{id: id, present: present}) }) })
		select {
		case d := <-reply:
			return d
		case <-c.Done():
			s.rt.Dispatch(func() { s.SetState(func() { s.withdrawHitl(id) }) })
			return runtime.ApprovalDecision{Allow: false, Reason: "interrupted"}
		}
	})

	s.sched = runtime.NewScheduler(s.w.ctx, schedObserver{onChange: func(_ string, q []string) {
		s.rt.Dispatch(func() { s.SetState(func() { s.queued = q }) })
	}}, time.Now)
	s.w.sess.SetSteerDrain(s.sched.DrainSteers)
	s.w.sess.SetSeparateDrain(s.sched.DrainSeparate)
	s.classifyKick = make(chan struct{}, 1)
	go s.w.sess.RunFollowupClassifier(s.w.ctx, s.sched, s.classifyKick)
	s.chat = s.w.sess.StartChat(s.w.ctx)

	if s.w.live {
		if w, _, e := term.GetSize(os.Stdin.Fd()); e == nil {
			s.width = w
			s.w.sess.SetWidth(w) // seed the diff renderer's width so the first edit's diff isn't clipped to the 95-col fallback
		}
		if s.w.introMatrix {
			s.intro = true
			s.introRecall = matrixRecall(s.w.ctx, s.w.sess)
			go s.runIntro()
		}
	}
	// Not logged in (locally known — no token file, or no memcode_ key in it):
	// open on the sign-in card instead of waiting for a doomed turn. Pure
	// state, so it applies headless too (the card renders after any intro).
	if !s.w.sess.Connected() {
		s.loginPrompting = true
	}

	// Background I/O (git stat, gateway model resolution) only under the real runner — in headless
	// tests there is no event loop to drain Dispatch, so these would race teardown.
	if s.w.live {
		s.refreshFooter()
		// Resolve the gateway's everyday (cheap-lane) model so the footer shows what actually runs
		// (e.g. glm-5p2) instead of the CLI's bootstrap identity (sonnet) before any turn.
		// Signed out this no-ops fast (no token → FetchModels errors); /login re-runs it.
		go s.resolveServingDefault()
		// Endpoint mode with no model configured anywhere: adopt the first model
		// the endpoint lists (persisted per-endpoint). No-op hosted/signed-out.
		go s.resolveEndpointModel()
		// Periodic footer refresh while idle: keeps the "N agents" count live and surfaces
		// done-notifications for dispatched sub-agents even when no turn is running. The
		// ticker is stopped when the app context is cancelled (s.w.ctx), so it dies with the
		// session — no orphan goroutine.
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-s.w.ctx.Done():
					return
				case <-t.C:
					s.refreshFooter()
				}
			}
		}()
		// Shell spinner: animates the footer's "N shell" segment while background
		// shells run, even when the app is otherwise idle. The running check happens
		// OFF the UI thread (registry mutex, no subprocess) and the tick only reaches
		// the UI when there's something to animate — zero Dispatch traffic when no
		// shells run. While busy, the turn spinner already advances s.spin, so this
		// one stands down (the !busy gate) instead of double-speeding the frames.
		go func() {
			t := time.NewTicker(150 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-s.w.ctx.Done():
					return
				case <-t.C:
					if len(s.w.sess.RunningShells()) == 0 {
						continue
					}
					s.rt.Dispatch(func() {
						if !s.busy() {
							s.SetState(func() { s.spin++ })
						}
					})
				}
			}
		}()
	}
}

func (s *appState) Dispose() { s.w.sess.EndChat(s.w.ctx) }

// appendChunk line-buffers engine output and commits whole lines to scrollback. Runs on the
// UI thread (via Dispatch).
func (s *appState) appendChunk(text string) {
	if out := s.absorbOutput(text); out != "" {
		s.ectx.AppendString(out + "\n")
	}
}

// flushAppend commits any trailing partial line at end of turn (responses rarely end in \n),
// styled through the same markdown path.
func (s *appState) flushAppend() {
	rem := s.appendBuf.String()
	s.appendBuf.Reset()
	if strings.TrimSpace(stripSGR(rem)) == "" {
		return
	}
	line := styleScrollbackLine(rem)
	if s.inCodeFence {
		line = renderCodeLine(rem)
	}
	if strings.ContainsRune(line, 0x1b) && !strings.HasSuffix(line, sgrReset) {
		line += sgrReset
	}
	s.ectx.AppendString(line + "\n")
	s.lastBlank = false
}

// sysln prints a front-end line (slash output, echoes) to scrollback. UI thread only.
func (s *appState) sysln(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.ectx.AppendString(strings.TrimRight(text, "\n") + "\n")
}

// refreshFooter recomputes branch + git diffstat + agent count off the render path, then
// redraws. It also detects sub-agents that finished since the last check and surfaces
// done-notification lines to scrollback (the "report back" for dispatched sub-agents).
func (s *appState) refreshFooter() {
	go func() {
		o := s.w.sess.Orientation(s.w.ctx)
		g := s.w.sess.GitStat(s.w.ctx)
		root := s.w.sess.Root()
		n := agentCount(root)
		s.rt.Dispatch(func() {
			// Detect finished agents on the UI thread (seenAgents is UI-thread state).
			// Seed the map on first call so already-running jobs don't all notify.
			if s.seenAgents == nil {
				s.seenAgents = seedSeenAgents(root)
			}
			// Hold report-backs while a decision card is up (plan selector / approval / ask):
			// injectTurn starts or queues a turn, which would wipe the pending plan selector
			// (planChoosing/advice) or race an open card. The drains REMOVE items, so we must
			// not call them when we can't process the result — skip this tick; they stay
			// pending in the registry and drain on a later tick once the card is gone.
			if !s.modalUp() {
				notes, backs := agentDoneNotifications(root, s.seenAgents)
				for _, note := range notes {
					s.sysln(note)
				}
				for _, b := range backs {
					// A background agent finished and asked to report back: feed its result into the
					// engine as a new turn so the model acts on it (not just a user notification).
					s.injectTurn(
						fmt.Sprintf("  ↳ agent %s finished — handing its result back to the agent", b.ID),
						fmt.Sprintf("[background agent %s finished] task: %s\n\nResult:\n%s", b.ID, b.Task, b.Result),
					)
				}
				// A foreground bash command that outran the turn budget was promoted to a
				// background shell; when it finishes, hand its result back as a new turn (same
				// contract as a report-back agent) so the model can act on the outcome.
				for _, sb := range s.w.sess.DrainShellReportBacks() {
					verb := "finished"
					if sb.Failed {
						verb = fmt.Sprintf("failed (exit %d)", sb.Exit)
					}
					s.injectTurn(
						fmt.Sprintf("  ↳ background shell %s — handing the result back to the agent", verb),
						sb.ReportPrompt(),
					)
				}
			}
			s.SetState(func() {
				s.branch = o.Branch
				s.gstat = g
				s.agents = n
			})
		})
	}()
}

// startTurn drains the scheduler's active transaction on a goroutine and marshals the
// busy/idle transitions + footer refresh back onto the UI thread.
// beginHitlWait marks the start of a human-in-the-loop wait (an ask/approval card just went up).
// The "Thinking…" clock measures LLM work, not how long the user takes to answer, so this span is
// excluded. Guarded so a second card raised while one is already up doesn't reset the start.
func (s *appState) beginHitlWait() {
	if s.hitlWaitAt.IsZero() {
		s.hitlWaitAt = time.Now()
	}
}

// endHitlWait closes the current HITL wait once NO card remains up, folding its duration into
// hitlWait. Called after a card is answered/dismissed; a no-op if another card is still pending.
func (s *appState) endHitlWait() {
	if s.hitlWaitAt.IsZero() || s.pending != nil || s.askReq != nil {
		return
	}
	s.hitlWait += time.Since(s.hitlWaitAt)
	s.hitlWaitAt = time.Time{}
}

// turnElapsed is the wall-clock since the turn began MINUS time blocked on HITL cards — i.e. actual
// LLM/processing time, which is what the "Thinking…" clock shows. Never negative.
func (s *appState) turnElapsed() time.Duration {
	d := time.Since(s.turnStart) - s.hitlWait
	if !s.hitlWaitAt.IsZero() {
		d -= time.Since(s.hitlWaitAt)
	}
	if d < 0 {
		d = 0
	}
	return d
}

// busy reports whether any owner holds the surface (a scheduler turn or an async op).
func (s *appState) busy() bool { return s.busyOwner != runtime.OwnerNone }

// setBusy is the ONE writer of busyOwner. Legal transitions are owner↔none; a
// direct turn↔async trample means two operations raced for the surface — the
// desync class behind the yolo wedge — so it is surfaced, not silently absorbed.
func (s *appState) setBusy(owner runtime.BusyOwner) {
	if owner != runtime.OwnerNone && s.busyOwner != runtime.OwnerNone && s.busyOwner != owner {
		s.sysln("⚠ busy-owner trample (turn/async raced) — please report this")
	}
	s.busyOwner = owner
}

// gateInput gathers the non-scheduler routing context for one Accept — the ONE
// place gate facts are collected (plan phase/epoch from the machine, busy owner).
func (s *appState) gateInput() runtime.GateInput {
	phase, epoch := s.w.sess.PlanPhaseEpoch()
	return runtime.GateInput{Phase: phase, CurrentEpoch: epoch, Busy: s.busyOwner}
}

// route reacts to a scheduler decision — the ONE place "Started → startTurn" lives
// (it was open-coded at seven sites). AwaitVerdict/PlanDeferred never reach here;
// acceptAndRoute and finalizePlanGate handle those explicitly.
func (s *appState) route(d runtime.Decision) {
	switch d.Kind {
	case runtime.DecisionStarted:
		s.startTurn()
	case runtime.DecisionBusyDeclined:
		// An async op (advisor, /compact) owns the surface and the scheduler is idle —
		// nothing was minted, so there is nothing to undo. Just tell the user.
		s.sysln("busy — finishing the current task first; resend in a moment")
	case runtime.DecisionSteered:
		// An explicit `+steer` — folds in at the next safe boundary.
		s.ectx.AppendTextLn([]ui.TextSpan{{Text: "  ↳ folding into the current task", Style: s.sty.muted}})
	case runtime.DecisionQueued, runtime.DecisionCoalesced:
		// A plain mid-turn follow-up: queued for now. Nudge the background classifier — it
		// batches the queue and folds in whatever refines the active task within ~30s.
		select {
		case s.classifyKick <- struct{}{}:
		default:
		}
	}
}

func (s *appState) startTurn() {
	tx, turnCtx, ok := s.sched.TakeActive()
	if !ok {
		return
	}
	in0, out0 := s.w.sess.Tokens()
	s.SetState(func() {
		s.setBusy(runtime.OwnerTurn)
		// A running turn means no plan decision is pending — drop the Execute selector so it
		// can't hang through a revise/steer-continuation. Turn-end re-raises it (below) ONLY if
		// the plan settles ready again, so the prompt tracks the plan state, not a stale snapshot.
		s.planChoosing = false
		s.planChoice, s.planStage, s.planAdvice, s.planCommitAsk = 0, 0, "", false
		s.turnStart = time.Now()
		s.hitlWait, s.hitlWaitAt = 0, time.Time{} // fresh clock; exclude HITL waits this turn
		s.turnIn0 = in0
		s.turnOut0 = out0
	})
	go func() {
		s.w.sess.RunTransaction(turnCtx, s.chat, tx)
		res := runtime.TransactionResult{State: runtime.TxCompleted}
		if turnCtx.Err() != nil {
			res.State = runtime.TxCancelled
		}
		promoted := s.sched.Finish(res)
		// A completed plan turn that produced a plan → raise the selector (or auto-execute in yolo).
		planReady := s.w.sess.Planning() && s.w.sess.PlanPresentable()
		yolo := planReady && s.w.sess.PlanYolo()
		// Dirty-tree check runs HERE (off the UI thread, once per raise) — it shells out to git.
		commitAsk := planReady && !yolo && s.w.sess.CommitGateNeeded(s.w.ctx)
		s.rt.Dispatch(func() {
			s.flushAppend()
			s.SetState(func() {
				s.setBusy(runtime.OwnerNone)
				if planReady && !yolo {
					s.planChoosing = true
					s.planChoice = 0
					s.planStage = 0 // a freshly proposed/revised plan → advisor on offer again
					s.planAdvice = ""
					s.planCommitAsk = commitAsk
				}
			})
			s.refreshFooter()
			if yolo {
				s.planExecute() // yolo: skip the selector, run the plan
			}
			// Run the promoted queued tx too. In yolo, Finish already promoted a follow-up
			// queued during planning into the active slot, so planExecute's instruction
			// queued BEHIND it and started nothing — without this the engine sat idle
			// forever (the /yolo + queued-follow-up hard stall). Guard on !busy so we never
			// double-start when planExecute already kicked a turn (the non-promoted path).
			if promoted && !s.busy() {
				s.startTurn()
			}
		})
	}()
	s.startSpinner()
}

// submit routes a submitted composer line: slash command, or a chat turn (echoed first).
// injectTurn feeds a NON-user-typed turn into the engine — a finished background agent's result.
// It shows a muted marker (not a user-prose echo), then routes the text through the scheduler
// exactly like submit (so it Starts now if idle, or queues behind the active turn) and kicks the
// executor. Runs on the UI thread (the completion poll), where Scheduler.Accept is safe.
func (s *appState) injectTurn(marker, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.ectx.AppendTextLn([]ui.TextSpan{{Text: marker, Style: s.sty.muted}})
	s.ectx.AppendString("\n")
	s.lastBlank = true
	// Same intake as a user submit — which also gives report-backs the async-busy
	// guard and the plan gate they were missing (a report-back injected mid-planning
	// used to bleed straight into the plan turn).
	s.acceptAndRoute(text)
}

// acceptAndRoute hands full to the transaction scheduler and reacts to the routing
// decision. The decision itself (steer/queue/defer/decline — including the plan gate
// and the async-busy collision) is decideIntake's, computed on the scheduler actor;
// this only performs the UI effects. AwaitVerdict means "classify first": the verdict
// re-enters through a second Accept in finalizePlanGate, keyed to the plan epoch it
// was computed against.
func (s *appState) acceptAndRoute(full string) {
	d := s.sched.Accept(full, s.gateInput())
	if d.Kind == runtime.DecisionAwaitVerdict {
		s.ectx.AppendTextLn([]ui.TextSpan{{Text: "  ↳ checking relevance to the plan…", Style: s.sty.muted}})
		s.classifyPlanGate(full)
		return
	}
	s.route(d)
}

// classifyPlanGate runs the plan-relevance classifier off the UI thread and finalizes
// with the verdict. The snapshot (task/draft/epoch) is taken NOW, on the UI thread;
// the verdict is validated against the CURRENT epoch at finalize time, so a verdict
// computed against a plan that has since exited or been replaced is void.
func (s *appState) classifyPlanGate(full string) {
	task, draft := s.w.sess.PlanGateSnapshot()
	_, submitEpoch := s.w.sess.PlanPhaseEpoch()
	go func() {
		related, title := s.w.sess.ClassifyPlanMessage(s.w.ctx, full, task, draft)
		s.rt.Dispatch(func() { s.finalizePlanGate(full, related, title, submitEpoch) })
	}()
}

// finalizePlanGate re-enters the scheduler with the verdict. The staleness math is
// the decision core's: a stale epoch while a NEW plan is drafting → AwaitVerdict
// again (re-classify against the new plan, never apply plan A's verdict to plan B);
// a stale epoch with planning over → normal routing (the message must not strand).
func (s *appState) finalizePlanGate(full string, related bool, title string, submitEpoch int) {
	gate := s.gateInput()
	gate.PlanEpoch = submitEpoch
	gate.Verdict = runtime.VerdictSeparate
	if related {
		gate.Verdict = runtime.VerdictRelated
	}
	switch d := s.sched.Accept(full, gate); d.Kind {
	case runtime.DecisionAwaitVerdict:
		s.classifyPlanGate(full) // a different plan now — judge against it
	case runtime.DecisionPlanDeferred:
		s.w.sess.DeferWhilePlanning(full, title)
		s.sysln("◇ separate — queued to run after the plan")
	default:
		s.route(d)
	}
}

func (s *appState) submit(line string) {
	// `line` carries [pasted …] tokens; the ENGINE gets the full expanded content, the ECHO
	// shows a clipped preview so scrollback stays tidy.
	full := s.expandPastes(line)
	pastes := s.pastes
	t := strings.TrimSpace(full)
	s.SetState(s.clearComposerInput)
	if t == "" {
		return
	}
	// Use the EXPANDED text (t), not the raw line: a slash command whose args include a
	// paste (e.g. `/plan <pasted spec>`) must get the real content, not the `[pasted #n]`
	// token — the raw line still carries the placeholder (and s.pastes is cleared above).
	if strings.HasPrefix(t, "/") && isKnownSlash(t) {
		// Echo the typed command into scrollback BEFORE dispatching — same prompt style as a
		// chat turn — so `/model`, `/theme`, etc. leave a trace of what was invoked instead of
		// only the bare confirmation line ("model → sonnet" with no idea what command ran it).
		s.ectx.AppendTextLn([]ui.TextSpan{
			{Text: composerPrompt, Style: s.sty.muted},
			{Text: t, Style: s.sty.user},
		})
		s.lastBlank = false
		if s.runSlash(t) {
			s.ectx.Quit()
		}
		return
	}
	// `$`/`>` shell lane: a LOCAL capture action, like a slash command — run it NOW, never queue it
	// behind the agent's turn. runShell renders its own `$ cmd` prompt + verbatim output (marshalled
	// onto the UI thread), so there's no separate prose echo. Bypasses the scheduler entirely.
	// Deliberately usable signed out — the capture lane never touches the gateway.
	if input.Parse(full, s.w.sess.Root()).Route == input.Shell {
		go s.w.sess.RunShellLine(s.w.ctx, full)
		return
	}
	// Mandatory login: a free-text turn is a model call. Gate it BEFORE the echo
	// so signed-out submissions don't look like they were accepted.
	if !s.w.sess.Connected() {
		s.signedOutNotice()
		return
	}
	// Echo the user's message into scrollback: the prose in their accent voice, then each pasted
	// blob's preview BELOW it, muted and middle-elided. The ENGINE already has the full text
	// (s.expandPastes above) — this is purely how the paste is shown, decided here, not by the
	// data.
	prose, below := pasteEchoBelow(line, pastes, s.width)
	s.ectx.AppendTextLn([]ui.TextSpan{
		{Text: composerPrompt, Style: s.sty.muted},
		{Text: prose, Style: s.sty.user},
	})
	for _, pl := range below {
		s.ectx.AppendTextLn([]ui.TextSpan{{Text: pl, Style: s.sty.muted}})
	}
	s.ectx.AppendString("\n")
	s.lastBlank = true // scrollback ends blank — keep the stream pipeline from doubling it

	// The plan-mode intake gate (judge a submission's relevance to the plan before it
	// reaches the scheduler; `+steer` bypasses) lives in the decision core now —
	// acceptAndRoute gets an AwaitVerdict decision and runs the classify/finalize
	// two-phase. System-generated plan instructions (revise/advisor/apply) never reach
	// here — they don't go through this composer path.
	s.acceptAndRoute(full)
}

const (
	pasteInlineMaxLines = 5               // a paste of at most this many lines is inserted as-is (not collapsed)
	pasteInlineMaxBytes = 512             // …provided it's also under this size — keep "small" actually small
	maxPasteBytes       = 2 * 1024 * 1024 // hard cap on a single paste (~500K tokens): generous for big files/diffs/logs/reviews — memcode is built around large context windows — while still bounding a pathological multi-MB/GB paste from OOMing the TUI or single-handedly overflowing the turn. Beyond it → truncate + flag.
)

// shouldInlinePaste reports whether a paste is small enough to drop straight into the composer
// rather than collapse to a [pasted #n] chip: short pastes (≤ pasteInlineMaxLines, under the byte
// budget) read fine inline, so chipping them just hides content the user wants to see and edit.
func shouldInlinePaste(text string) bool {
	if len(text) > pasteInlineMaxBytes {
		return false
	}
	return strings.Count(strings.TrimRight(text, "\n"), "\n") < pasteInlineMaxLines
}

// commitPaste lands a finished bracketed paste into the composer:
//   - a pure image-path drop (one or many) → "[Image #n] …" chips;
//   - a small paste (≤ a few lines, see shouldInlinePaste) → inline;
//   - anything else → a compact [pasted #n SIZE] token (real content stashed, expanded at submit).
//
// Oversized pastes were capped during accumulation (maxPasteBytes); the token shows the TRUE
// size and the stashed content carries a truncation marker, so the user and the model both see
// that content was dropped rather than a huge blob silently bombing the context.
func (s *appState) commitPaste() {
	text := s.pasteBuf.String()
	total := s.pasteBytes
	s.pasteBuf.Reset()
	s.pasteBytes = 0
	if text == "" {
		return
	}
	// A paste during the /apikeys masked-entry stage IS the key — route it to
	// the masked buffer, never the composer (which echoes to scrollback).
	if s.apikeysEntering {
		s.SetState(func() { s.apikeysInput = append(s.apikeysInput, []rune(strings.TrimSpace(text))...) })
		return
	}
	if total > len(text) { // capped during accumulation
		sent := len(text)
		s.ectx.AppendString(fmt.Sprintf("  ⚠ large paste (%s) truncated to %s inline.\n", humanBytes(total), humanBytes(sent)))
		text += fmt.Sprintf("\n…(paste truncated: %s of %s sent)", humanBytes(sent), humanBytes(total))
	}
	s.SetState(func() {
		ins := text
		if chips := s.imageChips(text); chips != "" {
			ins = chips
		} else if !shouldInlinePaste(text) {
			if s.pastes == nil {
				s.pastes = map[string]string{}
			}
			s.pasteSeq++
			token := fmt.Sprintf("[pasted #%d %s]", s.pasteSeq, humanBytes(total))
			s.pastes[token] = text
			ins = token
		}
		runes := []rune(s.composer)
		s.composer = string(runes[:s.cursor]) + ins + string(runes[s.cursor:])
		s.cursor += len([]rune(ins))
		s.slashSel = 0
	})
}

// imageChips collapses a PURE image-path paste — one OR many dragged images, whitespace-
// separated — into "[Image #n] …" chips, stashing each path for expansion at submit. Returns
// "" when the paste isn't purely image paths (so prose, or text+path, falls through to the
// text/[pasted] handling). Each path becomes its own chip, so a 30-image drop reads as
// [Image #1] … [Image #30] and all 30 attach via input.Parse.
func (s *appState) imageChips(text string) string {
	imgs := input.ImageMatches(text, s.w.sess.Root())
	if len(imgs) == 0 {
		return ""
	}
	rest := text
	for _, p := range imgs {
		rest = strings.Replace(rest, p, "", 1)
	}
	if strings.TrimSpace(rest) != "" {
		return "" // mixed prose + paths → keep as text, don't swallow it into chips
	}
	if s.pastes == nil {
		s.pastes = map[string]string{}
	}
	chips := make([]string, 0, len(imgs))
	for _, p := range imgs {
		s.imageSeq++
		token := fmt.Sprintf("[Image #%d]", s.imageSeq)
		s.pastes[token] = p
		chips = append(chips, token)
	}
	return strings.Join(chips, " ")
}

// expandPastes replaces every [pasted …]/[Image #n] token with its real content (for the engine).
func (s *appState) expandPastes(line string) string {
	for token, content := range s.pastes {
		line = strings.ReplaceAll(line, token, content)
	}
	return line
}

// expandComposer is the ENGINE-BOUND text of the composer: every paste token replaced by its
// FULL content (never clipped — clipping is a display concern handled elsewhere), trimmed. The
// modal reply paths take their outbound text via composerReply (which wraps this); chat turns
// go through submit(). Without this a path ships the bare [pasted …] token and the model never
// sees the content.
func (s *appState) expandComposer() string {
	return strings.TrimSpace(s.expandPastes(s.composer))
}

// clearComposerInput drops whatever the user had typed/pasted: the composer text, the cursor,
// and the paste stash. The SINGLE definition of "reset the input" — call it inside a SetState
// closure. Every modal that closes resets through here so the next prompt starts clean and no
// stale paste stash leaks into the following turn.
func (s *appState) clearComposerInput() {
	s.composer = ""
	s.cursor = 0
	s.pastes = nil
}

// composerReply is the ONE way a modal (approval "tell", ask answer, plan revise) consumes a
// typed/pasted reply: it returns the engine-bound text — paste tokens expanded to full content —
// and resets the input. It returns "" when nothing was typed, so the caller falls back to the
// highlighted selection. Routing every modal's Enter through this is what keeps the three reply
// paths — which look identical to the user — from deriving their text differently; hand-rolling
// it (reading s.composer raw) is exactly what shipped the bare "[pasted #1 5.6KB]" token to the
// planner instead of the content.
func (s *appState) composerReply() string {
	reply := s.expandComposer()
	if reply != "" {
		s.SetState(s.clearComposerInput)
	}
	return reply
}

// pasteEchoBelow splits a submitted line into the prose to echo in the user's voice and the
// muted preview lines to render BELOW it. Each text paste is lifted OUT of the prose and shown
// beneath as a middle-elided, muted block — the full text already went to the engine, so this is
// display only. Image chips ([Image #n]) stay inline in the prose.
func pasteEchoBelow(line string, pastes map[string]string, width int) (prose string, below []string) {
	prose = line
	for token, content := range pastes {
		if strings.HasPrefix(token, "[Image #") {
			continue // image chips echo inline
		}
		prose = strings.ReplaceAll(prose, token, "")
		below = append(below, "  "+token)
		for _, l := range clipPasteMiddle(content, width) {
			below = append(below, "  "+l)
		}
	}
	return strings.TrimSpace(prose), below
}

// pasteKeyText is the literal text a key contributes inside a paste (newlines normalized).
func pasteKeyText(key vaxis.Key) string {
	if key.Text != "" {
		if key.Text == "\r" {
			return "\n"
		}
		return key.Text
	}
	switch key.Keycode {
	case vaxis.KeyEnter:
		return "\n"
	case vaxis.KeyTab:
		return "\t"
	}
	return ""
}

// humanBytes is a compact size label: "812B", "2.3KB".
func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1fKB", float64(n)/1024)
}

// clipPasteMiddle previews a paste for the scrollback echo with a MIDDLE elision — the first
// `head` lines, an "— N lines skipped —" marker, then the last `tail` lines — so BOTH ends
// survive (the old tail-chop hid the end and made it look like content was lost). Display only:
// the full text always reached the engine via expandPastes; this never touches that value.
func clipPasteMiddle(content string, width int) []string {
	const head, tail = 10, 4
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) <= head+tail+1 {
		return lines
	}
	out := append([]string{}, lines[:head]...)
	out = append(out, skippedRule(len(lines)-head-tail, width-2)) // −2 for the 2-space echo indent
	return append(out, lines[len(lines)-tail:]...)
}

// skippedRule renders the middle-elision marker as a full-width horizontal rule with the count
// centered ("──────── 28 lines skipped ────────"), so it reads as a clear separator instead of a
// short dash pair lost in the text. Falls back to the compact "— N lines skipped —" when the row
// is too narrow (or width is unknown) to draw a rule.
func skippedRule(n, width int) string {
	label := fmt.Sprintf(" %d lines skipped ", n)
	rl := len([]rune(label))
	if width < rl+4 {
		return "—" + label + "—"
	}
	dashes := width - rl
	left := dashes / 2
	return strings.Repeat("─", left) + label + strings.Repeat("─", dashes-left)
}

// menu returns the slash autocomplete matches when the composer is a bare "/prefix".
func (s *appState) menu() []slashCmd {
	if strings.HasPrefix(s.composer, "/") && !strings.ContainsRune(s.composer, ' ') {
		return matchSlash(s.composer)
	}
	return nil
}

func (s *appState) clampedSel(n int) int {
	if n == 0 {
		return 0
	}
	if s.slashSel < 0 {
		return 0
	}
	if s.slashSel >= n {
		return n - 1
	}
	return s.slashSel
}

func (s *appState) cycleMode() {
	next := permissions.ModeAsk
	switch s.w.sess.Mode() {
	case permissions.ModeAsk:
		next = permissions.ModeAuto
	case permissions.ModeAuto:
		next = permissions.ModeAllowAll
	case permissions.ModeAllowAll:
		next = permissions.ModeAsk
	}
	s.w.sess.SetMode(next)
	s.SetState(func() {})
}

// answerApproval replies to the blocked engine goroutine and clears the prompt.

// HandleEvent is the sole input handler. The composer is rendered (not a focusable TextField)
// and edited here, so no widget requests a hardware cursor — which is essential: a focused
// TextField parks the terminal cursor mid-region, and vaxis's primary-screen renderer then
// overshoots its region-erase upward and wipes committed scrollback (the vanishing-echo bug).
func (s *appState) HandleEvent(ctx ui.EventContext, ev ui.Event) ui.EventResult {
	// Resize: remember the terminal width for the intro card + push it to the session so the
	// diff renderer lays out at the real width, not the 95-column fallback (SetWidth is only
	// ever called here and at InitState — without it every diff clips to ~93 cells with …).
	if r, ok := ev.(ui.Resize); ok {
		s.SetState(func() {
			s.width = r.Cols
			s.w.sess.SetWidth(r.Cols)
		})
		return ui.EventIgnored
	}
	// Bracketed paste frames a burst of EventPaste keys between these two markers. Buffer the
	// burst whole — never let an embedded newline submit or split it into N commands.
	switch ev.(type) {
	case vaxis.PasteStartEvent:
		s.pasting = true
		s.pasteBuf.Reset()
		s.pasteBytes = 0
		return ui.EventHandled
	case vaxis.PasteEndEvent:
		s.pasting = false
		s.commitPaste()
		return ui.EventHandled
	}
	key, ok := ev.(vaxis.Key)
	if !ok {
		return ui.EventIgnored
	}
	// A key inside a paste (or a stray EventPaste key if the start marker was missed) is
	// literal content — accumulate it; commitPaste lands it as a [pasted …] token on PasteEnd.
	// Cap the buffer at maxPasteBytes so a 1GB paste can't OOM us or bomb the context; the TRUE
	// size is still tracked (pasteBytes) for the token label and the truncation marker.
	if s.pasting || key.EventType == vaxis.EventPaste {
		t := pasteKeyText(key)
		s.pasteBytes += len(t)
		if s.pasteBuf.Len() < maxPasteBytes {
			s.pasteBuf.WriteString(t)
		}
		return ui.EventHandled
	}
	// The kitty keyboard protocol sends press AND release for each key; act on press/repeat
	// only. Otherwise the release re-fires the action — e.g. Enter's press opens the theme
	// picker and its release immediately applies+closes it, so the selector never appears.
	if key.EventType == vaxis.EventRelease {
		return ui.EventIgnored
	}
	// Two bare Escapes in a row clear the composer; any other key disarms that
	// (so the second Esc must immediately follow the first).
	if key.String() != "Escape" {
		s.escArmed = false
	}
	// Ctrl+C is global: interrupt a running turn or async op, else quit — every mode/modal.
	if key.String() == "Ctrl+c" {
		switch {
		case s.asyncCancel != nil:
			s.asyncCancel() // a runAsync op (/compact //models/advisor) — cancel it (was a dead key)
		case s.busy():
			s.sched.Cancel()
		default:
			ctx.Quit()
		}
		return ui.EventHandled
	}
	// Matrix intro: any key ends it; a printable key seeds the composer (so typing to skip
	// isn't lost). The framework renders the card, so this is just an event — clean.
	if s.intro {
		s.SetState(func() {
			s.settleIntro() // skip the animation, but still rest on the settled glyph wordmark
			if key.Text != "" && key.Modifiers&(vaxis.ModCtrl|vaxis.ModAlt) == 0 {
				s.composer += key.Text
				s.cursor = len([]rune(s.composer))
			}
		})
		return ui.EventHandled
	}
	// Theme picker owns the keyboard while open.
	// Modal pickers own the keyboard while open; each handles its own keys in its component file.
	if s.themePicking {
		return s.handleThemeKey(key.String())
	}
	if s.modelPicking {
		return s.handleModelKey(key)
	}
	if s.extraMileChoosing {
		return s.handleExtraMileKey(key.String())
	}
	if s.rewindChoosing {
		return s.handleRewindKey(key.String())
	}
	if s.personalityChoosing {
		return s.handlePersonalityKey(key.String())
	}
	if s.loginPrompting {
		return s.handleLoginPromptKey(key.String())
	}
	if s.apikeysPicking {
		return s.handleApikeysKey(key)
	}
	if s.syncChoosing {
		return s.handleSyncKey(key.String())
	}

	runes := []rune(s.composer)

	// Approval card: ↑↓/Tab move among Yes / don't-ask-again / tell; Enter confirms the
	// highlighted outcome (or, when anything's typed, deny + tell the agent differently);
	// Esc stops the turn. Other keys fall through to the composer so feedback can be typed.
	if s.pending != nil {
		switch key.String() {
		case "Up":
			if s.approveChoice > 0 {
				s.SetState(func() { s.approveChoice-- })
			}
			return ui.EventHandled
		case "Down", "Tab":
			if s.approveChoice < len(s.approvalOptions())-1 {
				s.SetState(func() { s.approveChoice++ })
			}
			return ui.EventHandled
		case "Enter":
			fb := s.composerReply()
			opt := s.approvalOptions()[s.approveChoice]
			switch approvalEnterAction(opt.kind, fb != "") {
			case "tell":
				s.answerApprovalChoice("tell", "", fb)
			case "hint":
				// "tell" highlighted but nothing typed — do NOT interrupt the turn on an empty
				// Enter (the old bug: it sent Interrupt and stopped the turn unexpectedly). "tell"
				// only makes sense WITH feedback; Esc is the explicit "stop".
				s.sysln("type what to tell the agent, then Enter — or pick an option with ↑↓")
			default: // "yes" / "remember" / "scope" / "cancel"
				s.answerApprovalChoice(opt.kind, opt.scope, "")
			}
			return ui.EventHandled
		case "Escape":
			if s.pending.Label == "Use skill" {
				// Skill cards: Esc = Skip — plain deny, the turn continues (matches the Skip row).
				s.answerApprovalChoice("cancel", "", "")
			} else {
				// "No, stop." — interrupt the whole turn, not just this one action.
				s.answerApproval(runtime.ApprovalDecision{Interrupt: true})
			}
			return ui.EventHandled
		case "Shift+Tab":
			// Deliberate hotkey path to allow-all even with a card open; landing there
			// means "stop asking", so it also answers the pending prompt.
			s.cycleMode()
			if s.w.sess.Mode() == permissions.ModeAllowAll {
				s.answerApproval(runtime.ApprovalDecision{Allow: true})
			}
			return ui.EventHandled
		}
		// any other key falls through to composer editing (feedback for "tell")
	}

	// Ask card: ↑↓/Tab move among the offered answers; Enter picks the highlighted one (or
	// sends typed text, if any); Esc skips (the agent proceeds on best judgment). Other keys
	// fall through to the composer so a free-form answer can be typed.
	if s.askReq != nil {
		n := len(s.askReq.Options)
		switch key.String() {
		case "Up":
			if s.askChoice > 0 {
				s.SetState(func() { s.askChoice-- })
			}
			return ui.EventHandled
		case "Down", "Tab":
			if s.askChoice < n-1 {
				s.SetState(func() { s.askChoice++ })
			}
			return ui.EventHandled
		case "Enter":
			if typed := s.composerReply(); typed != "" { // expanded reply (paste tokens → content), input reset
				s.answerAsk(typed)
			} else if n > 0 {
				s.answerAsk(s.askReq.Options[s.askChoice].Label)
			} else {
				s.answerAsk("") // no options offered and nothing typed → skip
			}
			return ui.EventHandled
		case "Escape":
			s.answerAsk("")
			return ui.EventHandled
		}
		// any other key falls through to composer editing (free-form answer)
	}

	// Plan selector: ↑↓ choose, Enter selects (or revises if you've typed), Esc cancels.
	// Other keys fall through to the composer so you can type a revision.
	if s.planChoosing {
		switch key.String() {
		case "Up":
			if s.planChoice > 0 {
				s.SetState(func() { s.planChoice-- })
			}
			return ui.EventHandled
		case "Down":
			if opts, _ := s.planOptions(); s.planChoice < len(opts)-1 {
				s.SetState(func() { s.planChoice++ })
			}
			return ui.EventHandled
		case "Enter":
			// A typed OR pasted revision always means "revise with this" — through the
			// SAME composerReply helper as the approval/ask paths, so the three reply
			// sinks (which look identical to the user) can't derive their text differently.
			if rev := s.composerReply(); rev != "" {
				s.planRevise(rev)
				return ui.EventHandled
			}
			if _, acts := s.planOptions(); s.planChoice < len(acts) {
				acts[s.planChoice]()
			}
			return ui.EventHandled
		case "Escape":
			s.planCancel()
			return ui.EventHandled
		}
	}

	// Slash menu navigation takes the arrow/Tab/Enter keys while it's open.
	if menu := s.menu(); len(menu) > 0 {
		sel := s.clampedSel(len(menu))
		switch key.String() {
		case "Up":
			if sel > 0 {
				s.SetState(func() { s.slashSel = sel - 1 })
			}
			return ui.EventHandled
		case "Down":
			if sel < len(menu)-1 {
				s.SetState(func() { s.slashSel = sel + 1 })
			}
			return ui.EventHandled
		case "Tab":
			s.SetState(func() {
				s.composer = menu[sel].name + " "
				s.cursor = len([]rune(s.composer))
				s.slashSel = 0
			})
			return ui.EventHandled
		case "Enter":
			s.submit(menu[sel].name)
			return ui.EventHandled
		}
	}

	// @-file mention picker: takes ↑↓/Tab/Enter while open; other keys fall
	// through so typing narrows the query (see filemention.go).
	if fm := s.fileMenu(); len(fm) > 0 {
		if s.fileMenuKey(key.String(), fm) {
			return ui.EventHandled
		}
	}

	// Shift is meaningless for cursor/editing keys (the composer has no text selection), but the
	// keyboard protocol reports e.g. "Shift+BackSpace" when Shift is held — common when typing
	// capitals then deleting. Normalize those so they still work; Shift+Tab / Shift+Enter stay
	// distinct (handled below) because there Shift IS meaningful.
	ks := key.String()
	if rest, ok := strings.CutPrefix(ks, "Shift+"); ok {
		switch rest {
		case "BackSpace", "Delete", "Left", "Right", "Home", "End":
			ks = rest
		}
	}
	switch ks {
	case "Ctrl+c":
		if s.busy() {
			s.sched.Cancel()
		} else {
			ctx.Quit()
		}
		return ui.EventHandled
	case "Escape":
		// The status row promises "esc to interrupt" while busy — but with no
		// card/picker/menu open (all handled above, each owning its own Escape), this generic
		// switch was Esc's only remaining path, and it had no case for it: Esc was a dead key
		// mid-turn and only Ctrl+C actually stopped anything. Idle, Escape has no other meaning
		// here, so just swallow it (fall through to EventIgnored) rather than cancel nothing.
		if s.busy() {
			s.sched.Cancel()
			return ui.EventHandled
		}
		// Idle: two Escapes in a row clear the composer. The first arms (swallowed);
		// the second clears. With an empty composer there's nothing to clear.
		if s.composer != "" {
			if s.escArmed {
				s.SetState(func() {
					s.composer = ""
					s.cursor = 0
					s.slashSel = 0
					s.escArmed = false
				})
			} else {
				s.escArmed = true
			}
			return ui.EventHandled
		}
	case "Shift+Tab":
		s.cycleMode()
		return ui.EventHandled
	case "Shift+Enter", "Alt+Enter", "Ctrl+j":
		// Soft return: insert a literal newline for multi-line composing (Enter still
		// submits). Shift+Enter needs the kitty keyboard protocol (modern terminals);
		// Alt+Enter and Ctrl+J are the universal fallbacks where it isn't available.
		s.SetState(func() {
			s.composer = string(runes[:s.cursor]) + "\n" + string(runes[s.cursor:])
			s.cursor++
			s.slashSel = 0
		})
		return ui.EventHandled
	case "Enter":
		s.submit(s.composer)
		return ui.EventHandled
	case "Tab":
		s.tabCompletePath() // complete a bare path token against the repo file list; swallowed otherwise
		return ui.EventHandled
	case "BackSpace": // vaxis names it with a capital S
		if s.cursor > 0 {
			s.SetState(func() {
				s.composer = string(runes[:s.cursor-1]) + string(runes[s.cursor:])
				s.cursor--
				s.slashSel = 0
			})
		}
		return ui.EventHandled
	case "Delete":
		if s.cursor < len(runes) {
			s.SetState(func() {
				s.composer = string(runes[:s.cursor]) + string(runes[s.cursor+1:])
				s.slashSel = 0
			})
		}
		return ui.EventHandled
	case "Left":
		if s.cursor > 0 {
			s.SetState(func() { s.cursor-- })
		}
		return ui.EventHandled
	case "Right":
		if s.cursor < len(runes) {
			s.SetState(func() { s.cursor++ })
		}
		return ui.EventHandled
	case "Home":
		s.SetState(func() { s.cursor = 0 })
		return ui.EventHandled
	case "End":
		s.SetState(func() { s.cursor = len(runes) })
		return ui.EventHandled
	}
	// Printable input (ignore control-modified keys).
	if key.Text != "" && key.Modifiers&(vaxis.ModCtrl|vaxis.ModAlt|vaxis.ModSuper) == 0 {
		ins := key.Text
		s.SetState(func() {
			s.composer = string(runes[:s.cursor]) + ins + string(runes[s.cursor:])
			s.cursor += len([]rune(ins))
			s.slashSel = 0
		})
		return ui.EventHandled
	}
	return ui.EventIgnored
}

// Build assembles the live region: status · composer · footer, plus the slash menu and the
// approval prompt when present. WithDynamicPrimaryScreen sizes the region to this tree.
func (s *appState) Build(ctx ui.BuildContext) ui.Widget {
	if s.intro {
		return s.matrixIntroView()
	}
	if s.themePicking {
		return s.themePickerView()
	}
	if s.modelPicking {
		return s.modelPickerView()
	}
	if s.extraMileChoosing {
		return s.extraMilePickerView()
	}
	if s.rewindChoosing {
		return s.rewindPickerView()
	}
	if s.personalityChoosing {
		return s.personalityPickerView()
	}
	if s.loginPrompting {
		return s.loginPromptView()
	}
	if s.apikeysPicking {
		return s.apikeysPickerView()
	}
	if s.syncChoosing {
		return s.syncPickerView()
	}
	// Live-region rhythm: one blank line ABOVE each section — the status bar, the active
	// card/menu, the composer, the footer. The gap above the status bar comes from scrollback's
	// own trailing blank when it has one; otherwise (e.g. scrollback ends on a tool line) add it
	// here so the status bar never hugs the text above it. Exactly one blank either way.
	var rows []ui.Widget
	if !s.lastBlank {
		rows = append(rows, ui.SizedBox{Height: 1})
	}
	// While a question / approval card awaits the user, the turn is blocked on INPUT, not
	// computing — so hide the live status strip. A running "Thinking… 51m" spinner would imply
	// the model is burning time when it's just waiting for you (Claude Code likewise drops the
	// status bar while a prompt is up). The card below still renders.
	if s.statusStripVisible() {
		rows = append(rows, s.statusRow(), ui.SizedBox{Height: 1})
		// Live task panel directly below the status bar (Claude-Code style) — updates in place.
		if panel := s.todoPanel(); len(panel) > 0 {
			rows = append(rows, panel...)
			rows = append(rows, ui.SizedBox{Height: 1})
		}
	}

	switch {
	case s.pending != nil:
		rows = append(rows, s.approvalCard())
		rows = append(rows, s.hitlBadge()...)
		rows = append(rows, ui.SizedBox{Height: 1})
	case s.askReq != nil:
		rows = append(rows, s.askCard())
		rows = append(rows, s.hitlBadge()...)
		rows = append(rows, ui.SizedBox{Height: 1})
	case s.planChoosing:
		// Plan-ready selector directly above the composer (type to revise). Fresh plans offer the
		// advisor; once it's weighed in, the selector offers to fold its review into the plan.
		// Options and their actions come from ONE builder (planOptions) shared with the key handler.
		opts, _ := s.planOptions()
		head := "Plan ready."
		if s.planStage == 1 {
			head = "Advisor weighed in."
		}
		if s.planCommitAsk {
			head += " You have uncommitted changes."
		}
		planRows := []ui.Widget{ui.RichText{Spans: []ui.TextSpan{{Text: head, Style: s.sty.brand}}}}
		planRows = append(planRows, s.optionList(opts, s.planChoice, false)...)
		planRows = append(planRows, s.hintRow("↑↓ select · Enter · type to revise · Esc cancel"))
		rows = append(rows, s.card(planRows...), ui.SizedBox{Height: 1})
	default:
		// slash menu directly above the composer (↑↓ navigates, Tab completes, Enter runs).
		// The full filtered list is shown — the vaxis fork's region-grow fix makes a tall live
		// region scroll cleanly, so there's no longer a need to cap it to a scrolling window.
		menu := s.menu()
		sel := s.clampedSel(len(menu))
		for i, c := range menu {
			marker, nameStyle := "  ", s.sty.muted
			if i == sel {
				marker, nameStyle = "❯ ", s.sty.emph
			}
			rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
				{Text: marker, Style: s.sty.brand},
				{Text: c.name + "  ", Style: nameStyle},
				{Text: c.desc, Style: s.sty.muted},
			}})
		}
		if len(menu) > 0 {
			rows = append(rows, ui.SizedBox{Height: 1})
		}
		// @-file mention picker, same placement and affordances as the slash menu
		// (only one of the two can be open — see filemention.go).
		if fm := s.fileMenu(); len(fm) > 0 {
			fsel := s.clampedSel(len(fm))
			for i, f := range fm {
				marker, nameStyle := "  ", s.sty.muted
				if i == fsel {
					marker, nameStyle = "❯ ", s.sty.emph
				}
				rows = append(rows, ui.RichText{Spans: []ui.TextSpan{
					{Text: marker, Style: s.sty.brand},
					{Text: f, Style: nameStyle},
				}})
			}
			rows = append(rows, ui.SizedBox{Height: 1})
		}
	}

	rows = append(rows, s.composerRow(), ui.SizedBox{Height: 1}, s.footerRow())

	// MainAxisSizeMin shrink-wraps the live region to its content (otherwise it fills the whole
	// screen and there's no room for scrollback); CrossAxisStart left-aligns every row.
	return ui.Flex{
		Axis:               ui.Vertical,
		MainAxisSize:       ui.MainAxisSizeMin,
		CrossAxisAlignment: ui.CrossAxisStart,
		Children:           rows,
	}
}

// resolveServingDefault asks the gateway which model serves the everyday lane
// and updates the footer + persisted config. Called at boot and again after
// /login (the boot call no-ops while signed out). Telemetry ONLY — it must
// never drive the greeting or auth state: signed-in-ness at boot is decided
// LOCALLY by token presence (the /login-written file), and a dead token
// surfaces on first USE via the loop's ErrUnauthorized handler.
func (s *appState) resolveServingDefault() {
	fctx, cancel := context.WithTimeout(s.w.ctx, 2*time.Second)
	defer cancel()
	if info, err := provider.FetchModels(fctx); err == nil {
		s.defaultVendor = info.DefaultVendor()
		// Mirror the ladder's everyday lane (llm/lane.go, main_loop → {roles:
		// ["standard"], tier: "balanced"}): the configured standard role, else
		// the session vendor's balanced tier — a deployment that omits the
		// role still banners what will actually serve.
		std := info.Role("standard")
		if std == "" {
			vendor := s.w.sess.Vendor()
			if vendor == "" {
				vendor = info.DefaultVendor()
			}
			std = catalog.VendorTier(vendor, "balanced")
		}
		if std != "" {
			s.w.sess.SetServingDefault(std)
			if cfg, err := config.Load(s.w.sess.Root()); err == nil {
				cfg.ServingDefault = std // persist so NEXT launch's banner shows it immediately
				_ = cfg.Save()
			}
		}
	}
	s.rt.Dispatch(func() { s.SetState(func() {}) })
}

// startSpinner ticks the live region ~8fps while busy so the spinner + elapsed clock animate.
func (s *appState) startSpinner() {
	go func() {
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			done := make(chan bool, 1)
			s.rt.Dispatch(func() {
				if s.busy() {
					s.SetState(func() { s.spin++ })
				}
				done <- s.busy()
			})
			if !<-done {
				return
			}
		}
	}()
}

// runAsync runs a blocking session call (intel/compact/models — a model or network call) off the
// UI thread with the busy spinner, then prints its result (markdown-rendered) to scrollback. fn
// receives a CANCELABLE context so Ctrl+C can interrupt the op (see HandleEvent) — without it a
// hung /compact or /models against a dead gateway left the user with no working interrupt.
func (s *appState) runAsync(fn func(context.Context) string) {
	if s.busy() {
		s.sysln("busy — wait for the current task to finish")
		return
	}
	actx, cancel := context.WithCancel(s.w.ctx)
	in0, out0 := s.w.sess.Tokens()
	s.SetState(func() {
		s.setBusy(runtime.OwnerAsync)
		s.asyncCancel = cancel
		s.turnStart = time.Now()
		s.hitlWait, s.hitlWaitAt = 0, time.Time{} // fresh clock; exclude HITL waits this turn
		s.turnIn0 = in0
		s.turnOut0 = out0
	})
	s.startSpinner()
	go func() {
		out := fn(actx)
		s.rt.Dispatch(func() {
			interrupted := actx.Err() != nil // capture BEFORE cancel() (which would set Err)
			cancel()                         // release the ctx
			s.flushAppend()
			s.SetState(func() {
				s.setBusy(runtime.OwnerNone)
				s.asyncCancel = nil
			})
			s.refreshFooter()
			if interrupted {
				s.sysln("(interrupted)")
				return
			}
			s.printBlock(out)
		})
	}()
}

// printBlock appends multi-line text to scrollback, markdown-rendered + indented like prose.
func (s *appState) printBlock(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		r := mdToANSI(line)
		if strings.ContainsRune(r, 0x1b) && !strings.HasSuffix(r, sgrReset) {
			r += sgrReset
		}
		s.ectx.AppendString("  " + r + "\n")
	}
}

// --- shared types / helpers ---

type schedObserver struct {
	onChange func(activeID string, queued []string)
}

func (o schedObserver) SchedulerChanged(activeID string, queued []string) {
	if o.onChange != nil {
		o.onChange(activeID, queued)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
