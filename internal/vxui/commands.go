package vxui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/forks/vaxis/ui"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/theme"
)

const planExecuteInstruction = "Begin implementing the approved plan now, working its steps in order."

// runSlash dispatches a /command. Returns true if the app should quit.
// Aliases (slashAliases) resolve to their canonical command BEFORE the switch, so the map
// is the single source of truth for dispatch — add an alias there and it dispatches; no
// second case label to forget. The switch below only ever sees canonical names.
func (s *appState) runSlash(line string) (quit bool) {
	fields := strings.Fields(line)
	cmd := strings.ToLower(fields[0])
	if canon, ok := slashAliases[cmd]; ok {
		cmd = canon
	}
	args := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
	// Mandatory login: gateway-backed commands prompt for /login instead of
	// dispatching. Local commands (the slash catalog's whitelist) always work.
	if !s.w.sess.Connected() && !isLocalSlash(cmd) {
		s.signedOutNotice()
		return false
	}
	switch cmd {
	case "/quit":
		return true
	case "/help":
		s.sysln(slashHelp(s.w.sess.Restricted()))
	case "/login":
		s.loginSlash()
	case "/logout":
		s.logoutSlash()
	case "/mode":
		switch args {
		case "":
			s.cycleMode()
		case "ask", "auto", "allow-all":
			s.w.sess.SetMode(permissions.Mode(args))
		default:
			s.sysln("usage: /mode ask|auto|allow-all")
			return false
		}
		s.sysln("mode → " + string(s.w.sess.Mode()))
	case "/model":
		s.modelSlash(args)
	case "/apikeys":
		s.apikeysSlash()
	case "/websites":
		s.websitesSlash()
	case "/artifacts":
		s.artifactsSlash()
	case "/theme":
		if args != "" {
			if theme.Set(args) {
				s.SetState(func() { s.sty = makeStyles(theme.Active().Palette) })
				s.persistTheme()
				s.sysln("theme → " + args)
			} else {
				s.sysln("unknown theme " + args)
			}
			return false
		}
		names := theme.Names()
		// Highlight (and revert-to, on Esc) the RESOLVED theme, not the persisted
		// choice: with "random" persisted, Chosen() would highlight the random row
		// and an Esc would re-roll a whole new theme instead of restoring this one.
		cur := theme.Active().Name
		sel := 0
		for i, n := range names {
			if n == cur {
				sel = i
			}
		}
		s.SetState(func() {
			s.themeNames = names
			s.themeSel = sel
			s.themeOrig = cur
			s.themePicking = true
		})
	case "/arch":
		s.sysln(s.w.sess.ArchDoc())
	case "/jobs":
		s.sysln(s.w.sess.JobsRender())
	case "/tail":
		s.sysln(s.w.sess.TailJobArg(args))
	case "/kill":
		s.sysln(s.w.sess.KillJobArg(args))
	case "/dispatch":
		s.dispatchSlash(args)
	case "/agents":
		s.agentsSlash(args)
	case "/status":
		in, out := s.w.sess.Tokens()
		model := s.w.sess.DisplayModel()
		if model == "" {
			model = s.w.sess.Model()
		}
		line := fmt.Sprintf("model %s · mode %s · branch %s · ↑%d/↓%d tokens",
			provider.ShortModel(model), s.w.sess.Mode(), s.branch, in, out)
		if s.w.sess.Pin() == "" {
			// Show the vendor only when it differs from the deployment default
			// (cached off /v1/models; "openai" until the first fetch lands).
			hidden := s.defaultVendor
			if hidden == "" {
				hidden = "openai"
			}
			if v := s.w.sess.Vendor(); v != "" && v != hidden {
				line += " · vendor " + vendorLabel(v)
			}
		}
		if cr, _ := s.w.sess.CacheStats(); cr > 0 {
			line += fmt.Sprintf(" · %s %d%%", cacheGlyph, cacheHitRate(in, cr))
		}
		if theme.Chosen() == "random" { // only when the roll is the ONE unknowable: name what it picked
			line += " · theme " + theme.Active().Name + " (random roll)"
		}
		s.sysln(line)
	case "/plan":
		s.planStart(args, false)
	case "/yolo":
		s.planStart(args, true)
	case "/cost", "/costp", "/costby":
		s.sysln(s.costDetail(cmd != "/cost"))
	case "/goal":
		if strings.TrimSpace(args) == "" {
			s.sysln("usage: /goal <what you're trying to do>")
			return false
		}
		if id, err := s.w.sess.AddObjective(s.w.ctx, args); err != nil {
			s.sysln(fmt.Sprintf("couldn't record goal: %v", err))
		} else {
			s.sysln(fmt.Sprintf("recorded objective %s: %s", id, args))
		}
	case "/personality":
		if strings.TrimSpace(args) == "" {
			s.openPersonality() // no arg → open the voice picker
			return false
		}
		s.setPersonalityArg(args) // explicit arg → set + persist directly
	case "/extramile":
		switch strings.ToLower(strings.TrimSpace(args)) {
		case "":
			s.openExtraMile() // no arg → open the on/off selector
			return false
		case "on", "1", "true", "yes":
			s.applyExtraMile(true)
		case "off", "0", "false", "no":
			s.applyExtraMile(false)
		default:
			s.sysln("/extramile takes on or off (or no arg to pick)")
		}
	case "/effort":
		switch strings.ToLower(strings.TrimSpace(args)) {
		case "":
			s.sysln("thinking effort: " + s.w.sess.EffortOverride() + "   (set with /effort off | medium | high | auto)")
		case "off", "medium", "high", "auto":
			s.applyEffort(strings.ToLower(strings.TrimSpace(args)))
		default:
			s.sysln("/effort takes off, medium, high, or auto (no arg shows the current setting)")
		}
	case "/clear":
		s.clearChat()
	case "/resume":
		s.resumeChat(args)
	case "/fork":
		s.forkChat(args)
	case "/rewind":
		s.openRewind(args)
	case "/next", "/recap", "/overview", "/doctor":
		// /doctor is whitelisted signed-out, but its full run is model-backed —
		// report the local picture instead of dispatching a doomed call.
		if cmd == "/doctor" && !s.w.sess.Connected() {
			s.sysln("doctor (signed out — local checks only):\n" +
				"  ✗ gateway: not signed in — run /login to connect\n" +
				"  · config: " + provider.GlobalEnvPath() + "\n" +
				"  (model-backed checks run after sign-in)")
			return false
		}
		name := strings.TrimPrefix(cmd, "/")
		s.runAsync(func(ctx context.Context) string {
			out, _ := s.w.sess.Intelligence(ctx, name, args)
			return out
		})
	case "/compact":
		s.runAsync(func(ctx context.Context) string { return s.w.sess.Compact(ctx, s.chat) })
	case "/advisor":
		// /advisor is a deliberate user action — async like /next /recap /overview /doctor.
		// The args ARE the question; empty args get the default "advise the best path forward"
		// prompt (handled inside AskAdvisor). Effort "high" — a second opinion should think hard.
		s.runAsync(func(ctx context.Context) string {
			advice, _ := s.w.sess.AskAdvisor(ctx, args, "high")
			return advice
		})
	case "/sync":
		s.openSync()
	case "/todos": // /todo is a pure alias (slashAliases → /todos), resolved above
		if len(s.todos) == 0 {
			s.sysln("no active tasks.")
			return false
		}
		s.sysln("Tasks " + s.todos.Summary())
		s.sysln(s.todos.Render("  "))
	case "/debug":
		in, out := s.w.sess.Tokens()
		cr, cw := s.w.sess.CacheStats()
		model := s.w.sess.Model()
		serving := s.w.sess.ServingModel()
		servedBy := s.w.sess.ServedBy()
		vendor := s.w.sess.Vendor()
		if vendor == "" {
			vendor = "openai (default)"
		}
		pin := s.w.sess.Pin()
		if pin == "" {
			pin = "none (Automatic)"
		}
		ctxTokens, win := s.w.sess.ContextTokens(), s.w.sess.ContextWindow()
		pct := 0
		if win > 0 {
			pct = ctxTokens * 100 / win
		}
		trace := "off"
		if os.Getenv("MEMCODE_TRACE") != "" {
			trace = "on"
		}
		lanes := ""
		for _, ln := range s.w.sess.Lanes() {
			label := provider.ServingLabel(ln.Name) + " subscription"
			if ln.Kind == "ownkey" {
				label = "your " + ln.Vendor + " key"
			}
			lanes += fmt.Sprintf("\n  lane %s → %s models", label, ln.Vendor)
		}
		s.sysln(fmt.Sprintf("session %s\n  model %s (serving %s, backend %s)\n  vendor %s\n  pin %s\n  mode %s%s\n  ↑%d ↓%d tokens · cache %d read / %d write (%d%% hit)\n  context %d/%d (%d%%)\n  wire trace %s",
			s.w.sess.SessionID(), model, serving, servedBy, vendor, pin, s.w.sess.Mode(), lanes, in, out, cr, cw, cacheHitRate(in, cr), ctxTokens, win, pct, trace))
	default:
		s.sysln("unknown command " + cmd + " — try /help")
	}
	return false
}

// costDetail formats session spend (the /cost output), optionally with the by-purpose breakdown.
func (s *appState) costDetail(byPurpose bool) string {
	in, out, cr, cw, usd := s.w.sess.Spend()
	if in+out+cr+cw == 0 {
		return "no spend yet this session."
	}
	ep, onEndpoint := s.w.sess.Endpoint()
	showUSD := costShowsUSD(onEndpoint, s.w.sess.Pin())
	// Money truth per serving path: lane turns bill NOTHING to memcode —
	// sub turns are $0 (the API-value estimate stays, labeled as
	// not-a-charge), own-key turns are the user's own provider dollars.
	onSub := onEndpoint && provider.SubscriptionEndpointName(ep.Name)
	var billed, subUSD, keyUSD float64
	for _, bs := range s.w.sess.SpendByBackend() {
		if _, kind, ok := provider.LaneBackendVendor(bs.Backend); ok {
			if kind == "sub" {
				subUSD += bs.USD
			} else {
				keyUSD += bs.USD
			}
			continue
		}
		billed += bs.USD
	}
	var b strings.Builder
	b.WriteString("session spend (estimate):\n")
	fmt.Fprintf(&b, "  ↑ input    %-8s  cache: %s read · %s write (%d%% hit)\n", fmtTokens(in), fmtTokens(cr), fmtTokens(cw), cacheHitRate(in, cr))
	fmt.Fprintf(&b, "  ↓ output   %-8s\n", fmtTokens(out))
	if onSub {
		fmt.Fprintf(&b, "  ~ cost     $0 · %s subscription (≈$%.2f API value)", provider.ServingLabel(ep.Name), usd)
	} else if subUSD > 0 || keyUSD > 0 {
		fmt.Fprintf(&b, "  ~ cost     $%.2f billed to memcode credits", billed)
		if subUSD > 0 {
			fmt.Fprintf(&b, "\n             $0 on your subscription (≈$%.2f API value)", subUSD)
		}
		if keyUSD > 0 {
			fmt.Fprintf(&b, "\n             ~$%.2f on your own API key (billed by the provider)", keyUSD)
		}
	} else if showUSD {
		fmt.Fprintf(&b, "  ~ cost     $%.2f", usd)
	} else {
		b.WriteString("  ~ cost     — (custom endpoint, model not in the rate catalog: token counts only)")
	}
	if byPurpose {
		b.WriteString("\n\n  by purpose:\n")
		for _, ps := range s.w.sess.SpendByPurpose() {
			fmt.Fprintf(&b, "  %-9s %2d call  ↑%-7s ↓%-7s", ps.Purpose, ps.Calls, fmtTokens(ps.In+ps.CacheRead), fmtTokens(ps.Out))
			if showUSD {
				fmt.Fprintf(&b, "  $%.2f", ps.USD)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// costShowsUSD decides whether /cost prints $ figures: always on the memcode
// backend; on a custom endpoint only when the session model is in the embedded
// catalog (a real rate card). An uncataloged local model would be priced off
// the defaults card — a fabricated figure for a bill nobody is sending — so
// token counts stand alone there.
func costShowsUSD(onEndpoint bool, model string) bool {
	return !onEndpoint || provider.CatalogKnows(model)
}

// clearChat ends the current conversation and starts a fresh one (new session id, empty history).
func (s *appState) clearChat() {
	if s.busy() {
		s.sysln("can't clear mid-turn — wait for the current task to finish")
		return
	}
	if s.w.sess.Planning() {
		s.w.sess.ExitPlan(s.w.ctx, false)
	}
	s.SetState(func() { s.planChoosing = false })
	s.w.sess.EndChat(s.w.ctx)
	s.chat = s.w.sess.StartChat(s.w.ctx)
	s.sysln("new session — conversation cleared (scroll up for history).")
}

// resumeChat re-enters a saved session with its full conversation history.
// Bare /resume picks the most recent OTHER session (so it's useful right after
// /clear or a fresh launch); /resume <id-or-prefix> targets one explicitly.
func (s *appState) resumeChat(ref string) {
	if s.busy() {
		s.sysln("can't resume mid-turn — wait for the current task to finish")
		return
	}
	root := s.w.sess.Root()
	id := ""
	if strings.TrimSpace(ref) == "" {
		for _, cand := range runtime.ResumableSessions(root) {
			if cand != s.w.sess.SessionID() {
				id = cand
				break
			}
		}
		if id == "" {
			s.sysln("no other session to resume — sessions become resumable once they have a turn")
			return
		}
	} else {
		var err error
		if id, err = runtime.ResolveSession(root, strings.TrimSpace(ref)); err != nil {
			s.sysln(err.Error())
			return
		}
	}
	if s.w.sess.Planning() {
		s.w.sess.ExitPlan(s.w.ctx, false)
	}
	s.SetState(func() { s.planChoosing = false })
	s.w.sess.EndChat(s.w.ctx)
	s.w.sess.SetResume(id)
	s.chat = s.w.sess.StartChat(s.w.ctx)
	s.sysln("↩ resumed " + id + " — the conversation continues where it left off.")
}

// forkChat copies a session's transcript (+ checkpoints) to a NEW session id and
// enters it — the original stays untouched and independently resumable. Bare /fork
// forks the current session; /fork <id-or-prefix> forks another. The episodic log is
// not duplicated (recall would double-count), so the fork starts its own from here.
func (s *appState) forkChat(ref string) {
	if s.busy() {
		s.sysln("can't fork mid-turn — wait for the current task to finish")
		return
	}
	root := s.w.sess.Root()
	src := s.w.sess.SessionID()
	if strings.TrimSpace(ref) != "" {
		var err error
		if src, err = runtime.ResolveSession(root, strings.TrimSpace(ref)); err != nil {
			s.sysln(err.Error())
			return
		}
	}
	// Fork FIRST: a failure (e.g. no turns saved yet) leaves the live session running.
	newID, err := runtime.ForkSession(root, src)
	if err != nil {
		s.sysln("nothing to fork yet — " + err.Error())
		return
	}
	if s.w.sess.Planning() {
		s.w.sess.ExitPlan(s.w.ctx, false)
	}
	s.SetState(func() { s.planChoosing = false })
	s.w.sess.EndChat(s.w.ctx)
	s.w.sess.SetResume(newID)
	s.chat = s.w.sess.StartChat(s.w.ctx)
	s.sysln("⑂ forked " + src + " → " + newID + " — this thread is now independent of the original.")
}

// planStart enters plan mode and runs the planning turn (research-only) through the scheduler.
// yolo auto-resolves HITL questions and auto-executes the plan (no selector).
func (s *appState) planStart(task string, yolo bool) {
	if s.busy() {
		s.sysln("can't start a plan mid-turn — wait for the current turn to finish")
		return
	}
	if strings.TrimSpace(task) == "" {
		s.sysln("usage: /" + map[bool]string{false: "plan", true: "yolo"}[yolo] + " <task>")
		return
	}
	// Leftovers from an uncleanly-abandoned prior plan run get carried forward, not
	// vanished: EnterPlan below nils the defer buffer as a last resort, so anything still
	// parked (e.g. a classify verdict that landed after that plan's drain) is captured
	// here and re-parked against the NEW plan (after EnterPlan, below) — it drains at this
	// plan's exit like any other separate ask. Starting them as turns here instead would
	// race the EnterPlan right after (the turn could see Active=true and run as a plan turn).
	leftovers := s.w.sess.DrainPlanDeferred()
	if yolo {
		s.w.sess.EnterPlan(s.w.ctx, runtime.WithYolo(), runtime.WithTask(task))
	} else {
		s.w.sess.EnterPlan(s.w.ctx, runtime.WithTask(task))
	}
	label := "◆ plan  "
	if yolo {
		label = "◆ yolo  "
	}
	s.ectx.AppendTextLn([]ui.TextSpan{
		{Text: label, Style: s.sty.info},
		{Text: task, Style: s.sty.user},
	})
	s.ectx.AppendString("\n")
	s.lastBlank = true
	s.route(s.sched.Accept(task, runtime.GateInput{Internal: true}))
	// Re-park the prior plan's leftovers against this plan (titles were already consumed
	// by their first defer; the raw-text fallback keeps them visible on the fresh list).
	for _, text := range leftovers {
		s.w.sess.DeferWhilePlanning(text, "")
	}
}

// drainPlanDeferred replays messages the intake gate parked during the plan — ONE
// function for every exit, sink policy from runtime.PlanDrainSink: after Execute the
// items queue behind the just-accepted apply instruction; after Cancel nothing is
// ahead so they start right away (route handles both — the scheduler decides). The
// carry-forward sink (new plan) stays in planStart, which must capture BEFORE
// EnterPlan wipes the buffer. Internal: these were already classified separate.
func (s *appState) drainPlanDeferred(exit runtime.PlanExit) {
	if runtime.PlanDrainSink(exit) == runtime.SinkCarryForward {
		return // planStart owns the capture/re-park two-step
	}
	for _, text := range s.w.sess.DrainPlanDeferred() {
		s.route(s.sched.Accept(text, runtime.GateInput{Internal: true}))
	}
}

// planExecute approves the plan (pins it as the contract) and runs the apply turn.
func (s *appState) planExecute() {
	s.w.sess.ExitPlan(s.w.ctx, true)
	s.SetState(func() { s.planChoosing = false })
	s.sysln("Plan approved. Executing in " + string(s.w.sess.Mode()) + " mode.")
	s.route(s.sched.Accept(planExecuteInstruction, runtime.GateInput{Internal: true}))
	// Replay whatever the plan-intake classifier parked as SEPARATE while this plan was
	// being drafted — they queue behind the apply instruction just Accepted above, so they
	// run once the plan's execution is actually done, exactly as the user asked.
	s.drainPlanDeferred(runtime.ExitExecute)
}

// planOptions builds the plan-ready selector: labels and their actions from ONE place, so the
// render and the key handler can never disagree on what each index does. When the selector was
// raised over a dirty tree (planCommitAsk), the Execute row splits into commit-first / without-
// committing variants — the commit decision rides the selector instead of a second card.
func (s *appState) planOptions() (opts []choice, acts []func()) {
	add := func(label string, act func()) {
		opts = append(opts, choice{label: label})
		acts = append(acts, act)
	}
	execRows := func(plain string) {
		if !s.planCommitAsk {
			add(plain, s.planExecute)
			return
		}
		add("Commit first, then execute", func() {
			s.w.sess.CommitGateChoice(s.w.ctx, true)
			s.planExecute()
		})
		add("Execute without committing", func() {
			s.w.sess.CommitGateChoice(s.w.ctx, false)
			s.planExecute()
		})
	}
	if s.planStage == 1 {
		add("Revise plan with advice", s.planReviseWithAdvice)
		execRows("Execute plan as is")
	} else {
		execRows("Execute")
		add("Ask an advisor", s.planAskAdvisor)
	}
	add("Cancel", s.planCancel)
	return opts, acts
}

// planAskAdvisor sends the live plan to the external advisor for a second opinion, then
// re-raises the selector at the "advised" stage. The advice is PROSE — it never enters the turn
// loop, so the live plan (lastText) is untouched and "Execute plan as is" runs the original.
func (s *appState) planAskAdvisor() {
	if s.busy() {
		s.sysln("busy — wait for the current task to finish")
		return
	}
	plan := strings.TrimSpace(s.w.sess.LastText())
	if plan == "" {
		s.sysln("no plan to review yet — propose one first.")
		return
	}
	q := "Review this implementation PLAN (it states its own task under \"Goal\"). Advise whether to " +
		"proceed, the single biggest risk, and anything missing or over-built. Be decisive.\n\n" +
		"=== PLAN ===\n" + plan
	in0, out0 := s.w.sess.Tokens()
	s.SetState(func() {
		s.planChoosing = false
		s.setBusy(runtime.OwnerAsync)
		s.turnStart = time.Now()
		s.turnIn0 = in0
		s.turnOut0 = out0
	})
	s.sysln("◆ advisor  reviewing the plan…")
	s.startSpinner()
	go func() {
		advice, _ := s.w.sess.AskAdvisor(s.w.ctx, q, "medium")
		commitAsk := s.w.sess.CommitGateNeeded(s.w.ctx) // recheck off the UI thread — the tree may have changed
		s.rt.Dispatch(func() {
			s.flushAppend()
			s.SetState(func() { s.setBusy(runtime.OwnerNone) })
			s.refreshFooter()
			s.printBlock(advice)
			s.SetState(func() {
				s.planAdvice = advice
				s.planStage = 1
				s.planChoosing = true
				s.planChoice = 0
				s.planCommitAsk = commitAsk
			})
		})
	}()
}

// planReviseWithAdvice spends a planning turn folding the advisor's review into the plan (the
// agent can research to verify the advice; the advisor can't). The refined plan becomes live.
func (s *appState) planReviseWithAdvice() {
	advice := strings.TrimSpace(s.planAdvice)
	if advice == "" {
		s.sysln("no advisor advice to apply.")
		return
	}
	instruction := "Revise the plan to incorporate this external review, keeping it grounded in the repo " +
		"(you may research to verify its suggestions before adopting them). Note anything you deliberately " +
		"reject and why.\n\n=== REVIEW ===\n" + advice
	s.SetState(func() { s.planChoosing = false; s.clearComposerInput() })
	s.sysln("◆ advisor  folding the review into the plan…")
	s.route(s.sched.Accept(instruction, runtime.GateInput{Internal: true}))
}

func (s *appState) planCancel() {
	s.w.sess.ExitPlan(s.w.ctx, false)
	s.SetState(func() { s.planChoosing = false })
	s.sysln("Plan cancelled.")
	// Nothing else is queued ahead of these on cancel — they start right away.
	s.drainPlanDeferred(runtime.ExitCancel)
}

// planRevise feeds free-text feedback as another planning turn (plan mode is still active).
func (s *appState) planRevise(feedback string) {
	s.SetState(func() {
		s.planChoosing = false
		s.clearComposerInput()
	})
	s.ectx.AppendTextLn([]ui.TextSpan{
		{Text: composerPrompt, Style: s.sty.muted},
		{Text: feedback, Style: s.sty.user},
	})
	s.ectx.AppendString("\n")
	s.lastBlank = true
	s.route(s.sched.Accept(feedback, runtime.GateInput{Internal: true}))
}
