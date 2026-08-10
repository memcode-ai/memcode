package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/todos"
	"github.com/memcode-ai/memcode/internal/wire"
)

// maxOverflowRetries bounds the reactive compact-and-retry within a single turn, so
// a prompt that can't be shrunk below the window (e.g. one giant tool result) fails
// cleanly instead of spinning.
const maxOverflowRetries = 2

// maxStreamRetries bounds the per-call retry on a transient gateway stream cut, so a
// genuinely-down gateway fails cleanly instead of looping.
const maxStreamRetries = 3

// maxStallRounds bounds the empty-turn resume nudge so a genuinely stuck turn still ends.
const maxStallRounds = 2

// stallNudge resumes a turn that returned empty while work is still pending.
const stallNudge = "You returned an empty response, but there are still pending steps. Don't stop here — pick the next pending todo and do it now, using what you've already gathered. If you're blocked or the plan turns out to be wrong, say so explicitly (don't stall silently)."

// maxApplyContinuations bounds NO-PROGRESS apply continuations (todo state unchanged since
// the last one). Continuations with real progress reset the count — a long plan may pause
// for prose many times; only spinning without advancing terminates the run.
const maxApplyContinuations = 3

// applyContinue drives the next contract step after the model paused for prose mid-apply.
// Completion of an apply is runtime-owned state (see the contract-continuation branch in
// runLoop), so this is the loop's answer to a pause — not a behavioral rule for the model.
const applyContinue = "Steps from the approved plan are still pending. If you stopped deliberately — a major deviation or a blocker — put it to the user with ask_user now. Otherwise continue with the next pending step, keeping the todo list's statuses current as you finish steps."

// todoSig fingerprints the todo list's progress state so apply continuations can tell
// advancing from spinning.
func todoSig(l todos.List) string {
	var b strings.Builder
	for _, it := range l {
		b.WriteString(it.Status)
		b.WriteByte(';')
	}
	return b.String()
}

// pendingTodos reports how many tracked todo items are not yet done/skipped — the signal
// that an empty turn mid-task is a STALL, not completion.
func pendingTodos(l todos.List) int {
	n := 0
	for _, it := range l {
		if it.Status != todos.StatusDone && it.Status != todos.StatusSkipped {
			n++
		}
	}
	return n
}

// planWrapUp is appended to the plan-mode system prompt once the research budget
// is spent: it forces the model to synthesize the plan from what it has.
// minPlanLen: a synthesis shorter than this isn't a plan — it's a stall stub (the
// model trying to 'check one more thing' with no tools). Triggers one regenerate.
const minPlanLen = 400

// planMaxTokens is the output budget for plan SYNTHESIS. The plan is the deliverable
// and "long, well-structured output is expected here" — and with adaptive thinking ON
// (EffortHigh) the thinking shares this budget, so the old 8192 truncated big plans
// mid-sentence (stop_reason=max_tokens). Synthesis is streamed, so a large cap is safe
// (no HTTP timeout); Opus tops out at 128K output. Generous on purpose.
const planMaxTokens = 32000

// mainMaxTokens is the output budget for ordinary (non-plan) turns. Left unset, the provider
// defaults to a tiny 4096 — and because adaptive thinking SHARES the output budget, a
// high-effort turn would spend the whole 4096 on HIDDEN reasoning and emit ZERO tool calls,
// so the turn "thinks hard and does nothing" even though it has every tool. (Plan synthesis
// already learned this — see planMaxTokens.) Reserve real room; a high-effort turn gets the
// full planMaxTokens since its thinking is the largest.
const mainMaxTokens = 16384

// loopIterCap is the per-turn iteration budget. Normal ask/auto turns keep the 200-turn soft cap.
// The HIGH ceiling (maxIterationsYolo — the pure runaway/token backstop, since the no-progress
// stall detector was removed for killing legitimate long work) applies to allow-all AND to the
// plan APPLY phase: executing a user-approved multi-phase plan IS that kind of long unattended
// run, and capping it at 200 stranded big plans mid-execution ("reached max iterations — resend")
// even though the user had approved the whole thing. Plan mode itself keeps the soft cap (its
// reflect gate governs it). An explicit s.iterCap (the bounded plan-review audit loop) wins.
func (s *Session) loopIterCap() int {
	if s.iterCap > 0 {
		return s.iterCap
	}
	if !s.planCtl.Planning() && (s.effectiveMode() == permissions.ModeAllowAll || s.planCtl.IsApplying()) {
		return maxIterationsYolo
	}
	return maxIterations
}

// buildSteerNote wraps drained mid-turn steer(s) in the instructional frame the model sees.
// Steers are, by definition, the RELATED case (the background classifier only folds a queued
// follow-up in as a steer when it judges it a refinement of the current task — see
// ClassifyFollowups/foldQueued) — so they belong folded together, not split into separate
// todo items; a todo per steer would be busywork for what's really one task. Several can
// still land in the SAME drain (an explicit `+message` alongside a batch the classifier just
// folded in one pass), so the note goes plural to name the count instead of describing a
// joined list as singular "it" — but the instruction stays "fold them all in", matching the
// singular case. (Genuinely SEPARATE/disparate requests are a different path entirely: they
// stay queued, not steered, and get tracked as their own todo — see RunFollowupClassifier.)
func buildSteerNote(steers []string) string {
	if len(steers) > 1 {
		return fmt.Sprintf("[steering — the user added %d notes while you were working. They are "+
			"high-priority refinements of/additions to the CURRENT task, not new future turns. Fold "+
			"them all into what you are doing, and open your next message with one short line "+
			"acknowledging what changed before continuing.]\n", len(steers)) +
			strings.Join(steers, "\n")
	}
	return "[steering — the user added this while you were working. It is a high-priority " +
		"refinement of the CURRENT task, not a new task. Fold it into what you are doing, and " +
		"open your next message with one short line acknowledging what changed (e.g. \"Got it — " +
		"switching to X\") before continuing.]\n" + strings.Join(steers, "\n")
}

// buildSeparateNote wraps drained SEPARATE follow-up text(s) (the classifier's "not
// related" verdict) in the FYI-toned frame the model sees. Unlike buildSteerNote, this is
// NOT "fold this in" — the request stays queued to run as its OWN turn later; the model is
// just being told it happened (a todo item was already added so it's tracked) and should
// briefly acknowledge it, then stay focused on the current task.
func buildSeparateNote(texts []string) string {
	if len(texts) > 1 {
		return fmt.Sprintf("[the user also asked %d separate thing(s) while you were working. They "+
			"are NOT refinements of the current task — added to the todo list to track, and will run "+
			"as their own turn later. No action needed now; briefly acknowledge it in your next reply, "+
			"then stay focused on what you're doing.]\n", len(texts)) + strings.Join(texts, "\n")
	}
	return "[the user also asked this separate thing while you were working. It is NOT a " +
		"refinement of the current task — added to the todo list to track, and will run as its " +
		"own turn later. No action needed now; briefly acknowledge it in your next reply, then " +
		"stay focused on what you're doing.]\n" + strings.Join(texts, "\n")
}

// runLoop drives the model↔tool loop on *messages until the model stops or the
// iteration cap is hit. Shared by single-shot Run and interactive Chat.
//
// Plan mode is special: research turns run on the cheap research model with their
// prose SUPPRESSED (the deliverable is the plan, not the "let me look at X"
// narration — tool/explore markers carry the UX). When research ends, the actual
// plan is produced by a DISTINCT call on the planner model with tools off (see
// synthesizePlan) — never by the research model just because it stopped calling
// tools.
func (s *Session) runLoop(ctx context.Context, sys promptSpec, messages *[]wire.Message) (iterations int, completed bool, err error) {
	committedOut := 0       // output tokens from completed model calls this turn (live ↓ counter)
	s.turn = newTurnState() // fresh per-turn loop state (heal/stall rounds, gather, editedPaths, servedLine, interrupted)
	s.planCtl.BeginTurn()   // a presented plan goes back to Researching — no plan rendered YET this turn
	// The phase THIS turn started in. execute_plan moves the machine Presented→Applying
	// mid-loop; the break below fires ONLY on that Planning→Applying edge. The chained
	// APPLY turn starts with Phase()==Applying already (not Planning), so it is immune to
	// the break by construction — it must NOT bail after one tool batch.
	startPhase := s.planCtl.Phase()
	overflowRetries := 0 // reactive compact-and-retry budget (per turn)
	streamRetries := 0   // transient stream-cut retry budget (per turn)
	// FOCUS DECAY: entering a new turn, clear the previous turns' stale tool
	// dumps once the transcript is heavy — before they're re-sent another N times.
	s.evictOnTurnStart(messages)
	// Persist per-turn gather telemetry (mode/reads/re-reads) so an expensive turn is
	// diagnosable from data later (/analyze). Only when the turn actually gathered.
	// The same counts feed the cross-turn HOT set so the evictor stops thrashing
	// paths the session keeps re-reading.
	defer func() {
		if s.turn.gather != nil && s.turn.gather.total > 0 {
			mode, budget := s.gatherMode()
			sum := s.turn.gather.summary(mode, budget)
			if s.scope != "" { // scout sub-agents tag their scope so /analyze can attribute over-reads
				sum["scope"] = s.scope
			}
			s.emit(ctx, events.KindGatherSummary, sum)
			s.noteHotPaths(s.turn.gather.byTarget)
		}
	}()
	iterCap := s.loopIterCap()
	for iterations = 1; iterations <= iterCap; iterations++ {
		// A mid-turn EnterPlan flips planCtl.Active, but `sys` was captured at turn start
		// (pre-plan, Mode "exec"/"chat"). Re-tag it to "plan" so the REST of this turn both
		// composes the plan doctrine AND routes as a plan turn — without this the next call
		// goes out as a non-plan high-effort turn and the gateway escalates it to Opus. Done
		// once (after the swap sys.mode == "plan"); finishPlan/reflectGate then see it too.
		if s.planCtl.Planning() && sys.mode != "plan" {
			sys = sys.withMode("plan")
		}
		if s.planCtl.Planning() && iterations >= maxIterations {
			// Runaway backstop — reflect once (gather user-only forks) then finish; no
			// more research rounds on a pathological loop.
			refl := s.reflectGate(ctx, *messages, &committedOut)
			return s.finishPlan(ctx, sys, messages, committedOut, iterations, refl)
		}
		// Thinking + tier are judged PER USER MESSAGE by the turn_intent classifier
		// (fired in scoreTurn; joined here, once, before the first model call):
		// trivial lookups run with no thinking on the cheap tier; deep work gets
		// full thinking on the heavy tier from turn ONE. The MODEL is the selection
		// ladder's call — the loop sends intent (purpose + effort + difficulty +
		// the risk hint) and llm resolves the label (ROUTING.md).
		s.joinTurnJudge(ctx)
		eff := s.turnEffort
		if s.turn.healRounds > 0 {
			// Stuck fixing its OWN broken edit — full thinking, AND the self_heal
			// risk takes it to the frontier tier this turn (it CONVERGES instead
			// of spiraling on the cheap model).
			eff = wire.EffortHigh
		}
		if s.planCtl.Planning() {
			// The executive planner — deciding what to research, judging sufficiency,
			// resolving forks — is hard reasoning on the reasoning tier. Think hard to
			// match the model (synthesis is already EffortHigh; this is the loop itself).
			eff = wire.EffortHigh
		}
		// Budget the output so thinking (which shares max_tokens) can't starve the tool calls.
		// High effort = the largest hidden reasoning → give it the full plan-sized ceiling;
		// it's a CEILING, not a target, so the model still stops when done.
		maxTok := mainMaxTokens
		if eff == wire.EffortHigh {
			maxTok = planMaxTokens
		}
		resp, err := s.complete(ctx, s.purpose, sys.request(wire.Request{
			Messages: *messages, Tools: s.toolDefs(),
			Effort: eff, MaxTokens: maxTok,
			Difficulty:  s.turnDifficulty,    // the judge's tier verdict → the ladder's difficulty input
			BillingLane: s.turnBillingLane(), // "" normally; "credits" after an explicit BYOK-failure consent
			// Escalation signals only the CLI can see (self-heal, room friction,
			// high-risk surfaces) — inputs to the CLI's own semantic ladder.
			RoutingHint: s.turnRoutingHint(),
		}), committedOut, s.planCtl.Planning()) // suppress prose while gathering in plan mode
		if err != nil {
			if ctx.Err() != nil { // the user cancelled this turn
				s.printf("\n■ interrupted.\n")
				return iterations, false, nil
			}
			// Reactive end of the routing ladder: the prompt overflowed the window on
			// every backend (the gateway already absorbs cheap-lane overflow up to Anthropic's
			// 1M). Compact HARD at this safe boundary — the last model call didn't append
			// anything, so *messages still ends on a complete turn — and retry. Bounded so
			// an irreducible prompt fails cleanly.
			if errors.Is(err, wire.ErrContextOverflow) && overflowRetries < maxOverflowRetries && !s.planCtl.Planning() {
				overflowRetries++
				if line, ok := s.compactMessages(ctx, messages, compactKeepRecentOnOverflow, false); ok {
					s.printf("%s\n", metaStyle.Render("  ⊙ context overflow — "+line+"; retrying"))
					continue
				}
			}
			// Transient TRANSPORT failure: the gateway stream was cut mid-call (a dropped
			// connection, or Cloud Run cutting the request at its timeout). The failed call
			// appended nothing and ran no tools (tools execute only after a SUCCESSFUL call),
			// so the history is intact — re-issue the SAME call from the SAME state. This is a
			// per-CALL retry, NOT a turn replay, so no action is duplicated. Bounded + brief
			// backoff so a persistently-down gateway still fails cleanly instead of dying
			// silently with work half-done.
			if errors.Is(err, wire.ErrStreamIncomplete) && streamRetries < maxStreamRetries {
				streamRetries++
				s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⊙ connection dropped mid-call — retrying (%d/%d)", streamRetries, maxStreamRetries)))
				select {
				case <-ctx.Done():
					return iterations, false, nil
				case <-time.After(time.Duration(streamRetries*2) * time.Second):
				}
				continue
			}
			// Credits exhausted: a user-action error, not a transient one. Surface
			// a friendly message and stop the turn — no retry (buying credits is
			// not something the loop can fix). The gateway returned 402 with code
			// "insufficient_credits"; the client mapped it to the sentinel. The
			// error is CONSUMED (a return would double-print as a raw "error:").
			if errors.Is(err, wire.ErrInsufficientCredit) {
				s.printf("\n■ %s\n", metaStyle.Render("Credits exhausted — visit https://memcode.ai/account/billing to top up"))
				return iterations, false, nil
			}
			// Subscription mandatory (2026-07-26): the gateway refuses every LLM
			// call for an unsubscribed org, BYOK included. User-action error —
			// consumed, no retry.
			if errors.Is(err, wire.ErrSubscriptionRequired) {
				s.printf("\n■ %s\n", metaStyle.Render("Subscription required — choose a plan at https://memcode.ai/account/billing"))
				return iterations, false, nil
			}
			// Negative balance locked the account: everything is refused (BYOK
			// included) until credits are added. Consumed, no retry.
			if errors.Is(err, wire.ErrAccountLocked) {
				s.printf("\n■ %s\n", metaStyle.Render("Account locked — your balance is negative. Add credits at https://memcode.ai/account/billing"))
				return iterations, false, nil
			}
			// The turn died on the user's OWN provider key. The doctrine's real
			// invariant is "never SILENTLY bill credits when the user's key was
			// supposed to serve" — so the ONLY path onto credits is an explicit,
			// consented, per-turn choice made right here. Headless/sub-agent
			// sessions keep fail-the-turn.
			if errors.Is(err, wire.ErrByokKeyFailed) {
				if s.consentCreditsRetry(ctx) {
					s.printf("\n%s\n", metaStyle.Render("⊙ retrying this turn on memcode credits — your key stays configured (/apikeys to fix or remove it)"))
					continue
				}
				s.printf("\n■ %s\n", metaStyle.Render("Your API key was rejected — fix or remove it with /apikeys"))
				return iterations, false, nil
			}
			// Token rejected (401): the SESSION is signed out (expired or revoked
			// key). Disconnect the provider so the front-end's signed-out gate
			// takes over (no more doomed dispatches), and say what fixes it.
			// ONE friendly line — the error is consumed here (returning it would
			// double-print as a raw "error:" too).
			if errors.Is(err, wire.ErrUnauthorized) {
				s.ClearCredentials()
				s.printf("\n○ %s\n", metaStyle.Render("Signed out — your saved login is no longer valid. Run /login to reconnect."))
				return iterations, false, nil
			}
			// Old-gateway fallback for ONE release: pre-2026-07-26 gateways send
			// the subscription 402 as plain text with no code, so the sentinel
			// above can't fire. Delete after the next gateway deploy is everywhere.
			if strings.Contains(err.Error(), "subscription inactive") {
				s.printf("\n■ %s\n", metaStyle.Render("Subscription required — choose a plan at https://memcode.ai/pricing"))
				return iterations, false, nil
			}
			return iterations, false, err
		}
		// This is the MAIN conversation call (full message history) — its input size
		// is the current context fill, which the footer's "ctx N%" meter reads. Only
		// set it here, not on classify/reflect calls (their tiny prompts would make the
		// meter flicker).
		s.recordServed(func(v *servedState) { v.ctxTokens = resp.InputTokens + resp.CacheReadTokens })
		committedOut += resp.OutputTokens
		if txt := strings.TrimSpace(resp.Text()); txt != "" {
			s.lastText = txt
		}
		uses := resp.ToolUses()
		// Tool-call-leak self-heal: a GLM-family model on streaming endpoints sometimes emits
		// its tool call as TEXT ("<tool_call>NAME<arg_key>…") instead of a parsed tool_use —
		// an upstream streaming-parser bug (vLLM #42400/#39757/#36857), worse near the context
		// limit. The loop then sees zero tool_uses and dead-ends. Detect it, show the model its
		// own failed turn, and ask it to re-issue a REAL call — bounded so a persistent leak
		// can't spin. (Mitigation; the cures are non-streamed tool turns + smaller context.)
		if len(uses) == 0 && s.turn.toolLeakRounds < maxToolLeakRounds {
			if name := detectLeakedToolCall(resp.Text()); name != "" {
				s.turn.toolLeakRounds++
				s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⓘ %q arrived as text, not a tool call — asking the model to re-issue it (round %d)", name, s.turn.toolLeakRounds)))
				*messages = append(*messages, wire.Message{Role: "assistant", Blocks: resp.Blocks})
				*messages = append(*messages, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: toolLeakNudge}}})
				continue
			}
		}
		if len(uses) == 0 {
			if !s.planCtl.Planning() {
				// CONTRACT CONTINUATION (apply): an approved plan's completion is a STATE the
				// runtime owns — every contract step done, or an explicit stop put to the user
				// via ask_user — never an inference from the model pausing for prose. A no-tool
				// response mid-contract is just a pause: the loop keeps driving the next
				// pending step. Progress-bounded, not round-bounded: a continuation that
				// doesn't change the todo state counts toward maxApplyContinuations (so a
				// model that can't advance still terminates); real progress resets the bound.
				if s.planCtl.IsApplying() {
					if pending := pendingTodos(s.todos); pending > 0 {
						if sig := todoSig(s.todos); sig != s.turn.lastTodoSig {
							s.turn.lastTodoSig, s.turn.applyConts = sig, 0
						}
						if s.turn.applyConts < maxApplyContinuations {
							s.turn.applyConts++
							s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⓘ %d plan step(s) pending — continuing", pending)))
							*messages = append(*messages, wire.Message{Role: "assistant", Blocks: nonEmptyAssistant(resp.Blocks)})
							*messages = append(*messages, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: applyContinue}}})
							s.emit(ctx, events.KindNote, map[string]any{"note": "apply_continuation", "pending": pending})
							continue
						}
						s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⚠ apply ended with %d step(s) still pending — the model stopped advancing", pending)))
					}
				}
				// COMPLETION GATE: don't let the agent declare done on top of a file it
				// broke. If anything it edited no longer parses, nudge it to fix forward
				// and keep looping. Bounded (healRounds) so an unfixable error can't spin.
				if nudge := s.brokenEditNudge(); nudge != "" && s.turn.healRounds < maxHealRounds {
					if s.turn.healRounds == 0 {
						s.turn.firstBreak = nudge // the failure evidence, before repair rewrites it
					}
					s.turn.healRounds++
					s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⓘ self-check — an edited file doesn't parse; fixing before finishing (round %d)", s.turn.healRounds)))
					*messages = append(*messages, wire.Message{Role: "assistant", Blocks: resp.Blocks})
					*messages = append(*messages, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: nudge}}})
					continue
				} else if nudge == "" && s.turn.healRounds > 0 && !s.turn.lessonDone {
					// The agent broke its own edits and REPAIRED them this turn — the
					// one moment failure and fix are both known. Distill the strategy
					// lesson (async; the turn doesn't wait on it).
					s.turn.lessonDone = true
					s.distillLesson(resp.Text())
				}
				// A no-tools, no-TEXT response must NOT silently end the turn (the "sat on
				// idle" hang): surface it so the user sees something happened and can resend,
				// instead of an empty turn that looks like a freeze.
				if strings.TrimSpace(resp.Text()) == "" {
					// STALL net: an empty turn while todos remain is not "done" — it's a stall
					// (the model degraded, often after over-gathering — the SDK-migration
					// failure). Nudge it to resume the next pending step rather than silently
					// completing with work unfinished. Bounded so a truly stuck turn still ends.
					if pending := pendingTodos(s.todos); pending > 0 && s.turn.stallRounds < maxStallRounds {
						s.turn.stallRounds++
						s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⓘ stalled with %d task(s) pending — resuming (round %d)", pending, s.turn.stallRounds)))
						// resp.Blocks is EMPTY on the stall path (no text, no tools). Appending an
						// empty assistant message poisons history — the API rejects a message with
						// no content on the NEXT turn (400). Use a placeholder so alternation holds.
						*messages = append(*messages, wire.Message{Role: "assistant", Blocks: nonEmptyAssistant(resp.Blocks)})
						*messages = append(*messages, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: stallNudge}}})
						s.emit(ctx, events.KindNote, map[string]any{"note": "transaction_stalled_resume", "pending": pending})
						continue
					}
					s.printf("\n%s\n", metaStyle.Render("(the model returned an empty turn — nothing to show; try resending or rephrasing)"))
				}
				// Keep the final text answer in the conversation history. Without this,
				// a tool-less response (the common case — the agent just replies in prose)
				// vanished from history, so the NEXT turn saw the prior question as
				// unanswered and re-answered it (e.g. re-describing a pasted screenshot
				// when later asked to "kill the shell"). Tool turns already append at the
				// bottom of the loop; the final text turn must too.
				*messages = append(*messages, wire.Message{Role: "assistant", Blocks: nonEmptyAssistant(resp.Blocks)})
				s.recordTurn(resp.Text(), nil) // final assistant message → episodic log
				return iterations, true, nil
			}
			// EXECUTIVE REFLECTION: the planner stopped — does it actually have enough?
			// Structured triage of the remaining unknowns decides: research more
			// (tool-answerable), ask the user (user-only), or synthesize. Honor the model's
			// OWN verdict: as long as it says it needs more research, let it — a big task
			// legitimately wants many rounds, and forcing synthesis early just yields a plan
			// it can't write (→ escalation). No arbitrary round cap; the loop's runaway guard
			// (maxIterations, above — plan mode hard-stops there) is the sole backstop against
			// a model that never converges.
			refl := s.reflectGate(ctx, *messages, &committedOut)
			if refl.wantsMoreResearch() {
				round := s.planCtl.NoteReflect()
				s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⓘ reflecting — gathering more before planning (round %d)", round)))
				*messages = append(*messages, wire.Message{Role: "assistant", Blocks: resp.Blocks})
				*messages = append(*messages, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: refl.researchNudge()}}})
				continue
			}
			return s.finishPlan(ctx, sys, messages, committedOut, iterations, refl)
		}
		*messages = append(*messages, wire.Message{Role: "assistant", Blocks: resp.Blocks})
		// Read-only calls in this turn run concurrently; mutating ones stay serial.
		// (Hooked: user pre_tool_use hooks can veto a call — see hooks.go.)
		results := s.executeBatchHooked(ctx, uses)
		*messages = append(*messages, wire.Message{Role: "user", Blocks: results})
		s.recordTurn(resp.Text(), uses) // episodic log: assistant text + meaningful actions

		// execute_plan fired this batch: ExitPlan just flipped the state machine into the
		// apply phase (Active→false, Applying→true). END the plan turn NOW — the chained
		// apply turn (runTurn's Applying branch) is the SINGLE sanctioned execution, run
		// from a clean context on the apply contract. Without this break the loop continues
		// with Active=false, so it falls through to the NORMAL branch with mutating tools
		// unlocked, the model obeys execute_plan's "implement the steps now" result, and the
		// work runs HERE too — then AGAIN in the chained apply turn (the double-execution /
		// re-verify-after-execute bug). The tool-result already told the model what's next.
		// Gate on the start phase: the break is the Planning→Applying EDGE, not the apply
		// turn itself (which STARTS in Applying — it must NOT bail after one batch).
		if startPhase.Planning() && s.planCtl.IsApplying() {
			return iterations, true, nil
		}

		// SAFE BOUNDARY for mid-turn steering: tool_use/tool_result are now paired and the
		// next model call hasn't started, so folding in the user's `+steer` here can't split
		// a tool pair. The scheduler recorded it the instant the user submitted; we apply it
		// to THIS active transaction's next iteration (a steer can't change an in-flight call
		// — that's cancel).
		// Mid-turn steers fold in here. A steer arrives two ways, both already on the active
		// transaction's steer list by now: an explicit `+message`, and a plain queued
		// follow-up the background classifier judged RELATED and promoted (see
		// Session.RunFollowupClassifier). Either way the drain injects it as a high-priority
		// refinement of the current task.
		if s.steerDrain != nil {
			if steers := s.steerDrain(); len(steers) > 0 {
				note := buildSteerNote(steers)
				*messages = append(*messages, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: note}}})
				s.printf("%s\n", metaStyle.Render("  ↳ steering — folding your note into this task"))
				// Log the steer to the CANONICAL log: it's real user input that shaped the
				// work. Without this, a mid-turn correction ("actually make it TLS 1.3 only")
				// was absent from events.jsonl, so focus/orientation/recall lost the refinement.
				for _, st := range steers {
					s.slog.Append(sessionlog.Record{Kind: sessionlog.KindUserMessage, Text: st})
				}
			}
		}

		// SAME safe boundary, the other drain: genuinely SEPARATE follow-ups (the
		// classifier's "not related" verdict) never touch the active transaction — they
		// just sat invisibly in the queue until their own turn ran. Track them on the todo
		// list now (so they're visible immediately, no model round-trip required) and give
		// the model a brief FYI so it can acknowledge in prose too.
		if s.separateDrain != nil {
			if activeText, activeTitle, items := s.separateDrain(); len(items) > 0 {
				s.noteSeparateRequests(ctx, activeText, activeTitle, items, messages)
			}
		}

		// In-turn context management: same safe boundary. A long agentic turn re-sends its
		// whole growing history every iteration, so bound it here (elide stale tool output,
		// then compact if still over) — this is what keeps a 40-tool-call turn from
		// ballooning to millions of tokens and degrading the model (the SDK-migration spiral).
		s.manageInTurnContext(ctx, messages)

		// The user chose to stop this turn (interrupt from an approval prompt).
		if s.turn.interrupted {
			s.turn.interrupted = false
			s.printf("\n■ stopped this turn at your request.\n")
			return iterations, false, nil
		}
	}
	ceiling := "max iterations"
	if iterCap == maxIterationsYolo {
		ceiling = "long-run turn ceiling"
		if s.planCtl.IsApplying() {
			ceiling = "plan-execution turn ceiling"
		}
	}
	s.printf("\n%s\n", metaStyle.Render(fmt.Sprintf(
		"⚠ reached the %s (%d turns) without finishing — resend to continue.", ceiling, iterCap)))
	return iterCap, false, nil
}

// unknown is one open question the executive surfaced while reflecting.
type unknown struct {
	Question   string      `json:"question"`
	Kind       string      `json:"kind"`        // tool_answerable | user_only | non_blocking
	Options    []AskOption `json:"options"`     // for user_only (label + optional description; tolerant of bare strings)
	NextAction string      `json:"next_action"` // for tool_answerable
}

// reflection is the executive's structured sufficiency judgment after research.
type reflection struct {
	Sufficient bool      `json:"sufficient"`
	Unknowns   []unknown `json:"unknowns"`
	Decision   string    `json:"decision"` // research_more | ask_user | synthesize
}

func (r reflection) wantsMoreResearch() bool {
	if r.Decision == "research_more" {
		return true
	}
	for _, u := range r.Unknowns {
		if u.Kind == "tool_answerable" {
			return true
		}
	}
	return false
}

// researchNudge turns the tool-answerable unknowns into a back-to-research prompt.
func (r reflection) researchNudge() string {
	var b strings.Builder
	b.WriteString("Not ready to plan yet — resolve these first (delegate to explore(), or use read/the inspect shell), then continue:\n")
	n := 0
	for _, u := range r.Unknowns {
		if u.Kind != "tool_answerable" {
			continue
		}
		n++
		if strings.TrimSpace(u.NextAction) != "" {
			fmt.Fprintf(&b, "- %s (try: %s)\n", u.Question, u.NextAction)
		} else {
			fmt.Fprintf(&b, "- %s\n", u.Question)
		}
	}
	if n == 0 {
		b.WriteString("- gather more detail on the areas you flagged as uncertain.\n")
	}
	return b.String()
}

// reflectionTool is the forced structured-output contract for the executive
// reflection — the same forced-tool pattern every judge uses (a reasoning
// model on the planner lane rambles before prose JSON; a forced tool call
// can't). decodeForcedTool keeps the prose fallback for endpoints that
// ignore tool_choice.
var reflectionTool = wire.ToolDef{
	Name:        "record_reflection",
	Description: "Record the structured sufficiency judgment for the research done so far.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"sufficient": map[string]any{"type": "boolean"},
			"decision":   map[string]any{"type": "string", "enum": []string{"research_more", "ask_user", "synthesize"}},
			"unknowns": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question":    map[string]any{"type": "string"},
						"kind":        map[string]any{"type": "string", "enum": []string{"tool_answerable", "user_only", "non_blocking"}},
						"next_action": map[string]any{"type": "string"},
					},
					"required": []string{"question", "kind"},
				},
			},
		},
		"required": []string{"sufficient", "decision"},
	},
}

// reflectGate runs the structured executive reflection. Best-effort: on any error or
// undecodable output it returns a "synthesize" decision so planning never stalls.
func (s *Session) reflectGate(ctx context.Context, messages []wire.Message, committedOut *int) reflection {
	resp, err := s.complete(ctx, llm.Reflect, promptSpec{mode: "reflect"}.request(wire.Request{
		Messages: messages, MaxTokens: 1024,
		Tools:      []wire.ToolDef{reflectionTool},
		ToolChoice: reflectionTool.Name,
	}), *committedOut, true) // suppressed — internal reflection, not shown
	if err != nil {
		return reflection{Decision: "synthesize"}
	}
	*committedOut += resp.OutputTokens
	var r reflection
	if !decodeForcedTool(resp, reflectionTool, &r) {
		return reflection{Decision: "synthesize"}
	}
	if len(r.Unknowns) > 6 {
		r.Unknowns = r.Unknowns[:6]
	}
	s.emit(ctx, events.KindPlanReflected, map[string]any{
		"sufficient": r.Sufficient, "decision": r.Decision, "unknowns": len(r.Unknowns)})
	return r
}

// planVerdict is the cross-model critic's structured judgment on a drafted plan. The
// reviewer must SHOW ITS WORK: Checked carries the load-bearing claims it actually verified
// against the code (with evidence), so an "ok" is grounded, not vibes. Routing still keys
// only on Verdict.
type planVerdict struct {
	Verdict  string `json:"verdict"`  // ok | revise | escalate
	Severity string `json:"severity"` // low | medium | high
	Summary  string `json:"summary"`  // one terse line: WHY this verdict — shown under the ⏺ ReviewPlan tool line
	Checked  []struct {
		Claim    string `json:"claim"`
		Status   string `json:"status"` // true | false | unverified
		Evidence string `json:"evidence"`
		File     string `json:"file"`
	} `json:"checked"`
	Issues []struct {
		Kind   string `json:"kind"`
		Detail string `json:"detail"`
	} `json:"issues"`
	Feedback string `json:"feedback"` // concrete fixes (for revise) / the core problem (for escalate)
}

// review kickoffs drive the two phases of the tooled review. The audit kickoff starts the
// read-only investigation loop; the verdict kickoff (tool-less) collects the strict-JSON verdict.
const (
	reviewAuditKickoff = "Sanity-check the plan above against the actual code — a FAST spot-check, not a full audit. " +
		"Pick only the 2-4 claims most likely to SINK the plan (the riskiest cited refs, or relied-upon state/lifecycle " +
		"assumptions) and confirm those with a couple of quick read_file/ripgrep calls each. Do NOT open every file or " +
		"re-derive the plan — this gates a build, so be quick. Then summarize just the few you checked as " +
		"claim → true/false → evidence (path:line)."
	reviewVerdictKickoff = "Now record your FINAL verdict by calling the record_verdict tool. Base it on the few claims " +
		"you spot-checked: if those held and nothing clearly broken surfaced, verdict \"ok\" — you do NOT need to have " +
		"verified everything. Keep \"summary\" to one terse line."
)

// recordVerdictTool is the structured-output channel for the review verdict: the verdict call
// FORCES this tool (ToolChoice), so the reviewer's decision arrives as schema-constrained tool_use
// input instead of best-effort prose-JSON. The schema mirrors planVerdict.
var recordVerdictTool = wire.ToolDef{
	Name:        "record_verdict",
	Description: "Record your final review verdict on the plan. Call this exactly once.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"verdict":  map[string]any{"type": "string", "enum": []string{"ok", "revise", "escalate"}, "description": "ok = sound; revise = fixable problem found; escalate = core approach wrong/unsafe"},
			"severity": map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
			"summary":  map[string]any{"type": "string", "description": "a brief headline for the verdict (one line)"},
			"checked": map[string]any{"type": "array", "description": "the few load-bearing claims you spot-checked", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"claim":    map[string]any{"type": "string"},
					"status":   map[string]any{"type": "string", "enum": []string{"true", "false", "unverified"}},
					"evidence": map[string]any{"type": "string", "description": "path:line or symbol"},
					"file":     map[string]any{"type": "string"},
				},
			}},
			"issues": map[string]any{"type": "array", "description": "List EACH concrete problem as its OWN entry (several for a revise) — they're shown one per line. Don't cram multiple findings into one.", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":   map[string]any{"type": "string"},
					"detail": map[string]any{"type": "string", "description": "one specific, file-cited finding, e.g. 'Step 10 passes cfg.Theme to tui.Run but Run's signature lacks it'"},
				},
			}},
			"feedback": map[string]any{"type": "string", "description": "concrete, directly-applicable fixes (for revise/escalate)"},
		},
		"required": []string{"verdict", "summary"},
	},
}

// parsePlanVerdict extracts the verdict via the shared decodeForcedTool (forced tool_use
// first, best-effort prose-JSON fallback). Returns ok=false when neither yields a verdict
// with a non-empty Verdict. The review call itself stays on sub.complete (it carries the
// audit transcript and the Review purpose) — only the decode is shared judge plumbing.
func parsePlanVerdict(resp wire.Response) (planVerdict, bool) {
	var v planVerdict
	if decodeForcedTool(resp, recordVerdictTool, &v) && v.Verdict != "" {
		return v, true
	}
	return planVerdict{}, false
}

// reviewWithTools runs the cross-model plan critic as a BOUNDED, read-only AUDIT: a forked
// sub-session (the reviewer model — DeepSeek — via purpose=review) gets file-read/grep/diff
// tools and VERIFIES the plan's load-bearing claims against the real code, then emits a
// strict-JSON verdict in a tool-less call (so the gateway forces clean JSON). Best-effort:
// any spawn/loop/parse error proceeds as {Verdict:"ok"} (a broken reviewer NEVER blocks
// planning) but is recorded as review_status=skipped so telemetry can tell "passed" from
// "did not run". Returns the verdict and the reviewer's served label (for the tool line).
// messages must end with the drafted plan so the reviewer can see it.
func (s *Session) reviewWithTools(ctx context.Context, messages []wire.Message) (planVerdict, string) {
	// The plan thread was produced on the cheap PLANNER (glm — OpenAI-style tool ids). Its
	// tool_use/tool_result plumbing means nothing to the REVIEWER (a different model, usually
	// Haiku/Anthropic), and replaying another backend's tool calls into Anthropic trips its
	// strict tool_use→tool_result validation ("tool_use ids without tool_result" → 400/502).
	// The reviewer audits the plan against the CODE with its OWN read-only tools, so hand it a
	// clean natural-language transcript (task + reasoning + plan), not the planner's tool calls.
	messages = stripToolBlocks(messages)
	sub := New(s.store, s.runner.Fork(), s.root, s.model, permissions.ModeAsk, io.Discard)
	sub.readOnly = true          // gates tools to the read-only whitelist (no edits, no bash)
	sub.purpose = llm.Review     // routes every call to the reviewer model + selects the review budget/whitelist
	sub.iterCap = reviewMaxTurns // bounded audit loop — a claim-verifier, not a full explorer

	skip := func(reason string) (planVerdict, string) {
		s.emit(ctx, events.KindPlanReviewed, map[string]any{"review_status": "skipped", "skip_reason": reason})
		// Surface the skip in the UI too (a ⎿ line) so it's never mistaken for a real "ok".
		return planVerdict{Verdict: "ok", Summary: "review unavailable — " + reason}, sub.ServedBy()
	}

	// Phase 1 — audit: investigate the plan against the code with read-only tools.
	auditMsgs := append(append([]wire.Message{}, messages...),
		wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: reviewAuditKickoff}}})
	if _, _, err := sub.runLoop(ctx, promptSpec{mode: "plan_review", facts: sub.baseFacts()}, &auditMsgs); err != nil {
		if ctx.Err() != nil {
			return planVerdict{Verdict: "ok"}, sub.ServedBy() // turn cancelled — silently accept, not a review failure
		}
		return skip("audit loop: " + err.Error())
	}

	// Phase 2 — verdict: FORCE the record_verdict tool so the JSON comes back structured
	// (schema-constrained tool_use input), reliable on Anthropic AND the cheap lane — instead
	// of best-effort prose-JSON, which a non-json_object model (e.g. Haiku) couldn't guarantee.
	auditMsgs = append(auditMsgs, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: reviewVerdictKickoff}}})
	resp, err := sub.complete(ctx, llm.Review, promptSpec{mode: "review", facts: sub.baseFacts()}.request(wire.Request{
		Messages: auditMsgs, Tools: []wire.ToolDef{recordVerdictTool}, ToolChoice: recordVerdictTool.Name,
	}), 0, true)
	if err != nil {
		if ctx.Err() != nil {
			return planVerdict{Verdict: "ok"}, sub.ServedBy()
		}
		return skip("verdict call: " + err.Error())
	}
	v, ok := parsePlanVerdict(resp)
	if !ok {
		return skip("unparseable verdict")
	}
	verified, refuted := 0, 0
	for _, c := range v.Checked {
		switch c.Status {
		case "true":
			verified++
		case "false":
			refuted++
		}
	}
	s.emit(ctx, events.KindPlanReviewed, map[string]any{
		"review_status": "ran", "verdict": v.Verdict, "severity": v.Severity,
		"checked": len(v.Checked), "verified": verified, "refuted": refuted,
	})
	return v, sub.ServedBy()
}

// stripToolBlocks returns a clean natural-language transcript: tool_use, tool_result, and
// thinking/redacted_thinking blocks are dropped, messages left empty are removed, and runs
// of same-role messages (which appear once the tool turns are gone) are merged so the thread
// still alternates. Used when handing a conversation to a DIFFERENT model than the one that
// produced it — another backend's tool ids/plumbing and reasoning scratch don't transfer,
// and replaying an unpaired tool_use into Anthropic is a hard 400 ("tool_use ids were found
// without tool_result blocks immediately after").
func stripToolBlocks(msgs []wire.Message) []wire.Message {
	out := make([]wire.Message, 0, len(msgs))
	for _, m := range msgs {
		var kept []wire.Block
		for _, b := range m.Blocks {
			switch b.Type {
			case "tool_use", "tool_result", "thinking", "redacted_thinking":
				// drop — the producing model's plumbing/scratch, meaningless to another model
			default:
				kept = append(kept, b)
			}
		}
		if len(kept) == 0 {
			continue
		}
		// Stripping tool turns leaves runs of same-role messages; Anthropic requires the
		// roles to alternate, so fold a same-role message into the previous one.
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Blocks = append(out[n-1].Blocks, kept...)
			continue
		}
		m.Blocks = kept
		out = append(out, m)
	}
	return out
}

// finishPlan is the plan-mode tail: ask the user-only forks (HITL), fold those answers
// plus any non-blocking risks into the synthesis system prompt, then synthesize the
// plan on the planner model. Forks are resolved BEFORE synthesis, never re-raised.
// Under yolo, HITL questions are auto-resolved as assumptions instead of asking.
func (s *Session) finishPlan(ctx context.Context, sys promptSpec, messages *[]wire.Message, committedOut, iterations int, refl reflection) (int, bool, error) {
	var decisions string
	if s.planCtl.Yolo() {
		decisions = s.autoResolveUnknowns(ctx, refl)
	} else {
		decisions = s.askUserOnly(ctx, refl)
	}
	if ctx.Err() != nil {
		s.printf("\n■ interrupted.\n")
		return iterations, false, nil
	}
	return s.synthesizePlan(ctx, sys.withExtra(decisions), messages, committedOut, iterations)
}

// askUserOnly drives an ask card for each user-only unknown (HITL) and returns a block
// for the synthesis system prompt: the answered decisions (build ON them) plus the
// non-blocking unknowns (note as risks). Skipped questions stay as open questions.
func (s *Session) askUserOnly(ctx context.Context, refl reflection) string {
	var b strings.Builder
	var asked bool
	if s.ask != nil {
		for _, u := range refl.Unknowns {
			if ctx.Err() != nil { // turn cancelled mid-questions — stop asking
				break
			}
			if u.Kind != "user_only" || strings.TrimSpace(u.Question) == "" {
				continue
			}
			if !asked {
				b.WriteString("\n\n[The user was asked these load-bearing decisions before planning — build the plan" +
					" ON the answers; do NOT re-raise them as open questions or ask again at the end. SKIPPED items" +
					" stay as explicit open questions]:\n")
				asked = true
			}
			resp := s.ask(ctx, AskRequest{Question: u.Question, Options: pruneEscapeOptions(u.Options)})
			ans := strings.TrimSpace(resp.Answer)
			if ans == "" {
				fmt.Fprintf(&b, "- %s → (skipped by the user — keep as an explicit open question)\n", u.Question)
				continue
			}
			s.emit(ctx, events.KindUserNote, map[string]any{"question": u.Question, "answer": ans})
			s.toolLine(true, "AskUser", u.Question, "", false)
			s.printf("%s\n", metaStyle.Render("  ⎿ "+clip(ans, 2000))) // echo the user's answer generously (model gets it in full)
			fmt.Fprintf(&b, "- %s → %s\n", u.Question, ans)
		}
	}
	var risks bool
	for _, u := range refl.Unknowns {
		if u.Kind != "non_blocking" || strings.TrimSpace(u.Question) == "" {
			continue
		}
		if !risks {
			b.WriteString("\n[Minor open questions to note under Risks — do NOT ask the user]:\n")
			risks = true
		}
		fmt.Fprintf(&b, "- %s\n", u.Question)
	}
	return b.String()
}

// autoResolveUnknowns replaces askUserOnly under yolo: instead of raising HITL
// questions, each user-only unknown is folded into the synthesis prompt as an
// auto-resolved assumption (no human available — make the recommended call). Tool-
// answerable unknowns were already resolved by the reflect gate's research loop;
// non-blocking unknowns are noted as risks, just like askUserOnly does. Coerces
// Decision=="ask_user" to "synthesize" defensively so no path accidentally re-prompts.
func (s *Session) autoResolveUnknowns(ctx context.Context, refl reflection) string {
	// Defensive: under yolo, ask_user makes no sense — coerce to synthesize.
	if refl.Decision == "ask_user" {
		refl.Decision = "synthesize"
	}
	s.emit(ctx, events.KindPlanReflected, map[string]any{"yolo": true, "decision": refl.Decision, "unknowns": len(refl.Unknowns)})

	var b strings.Builder
	var assumptions bool
	for _, u := range refl.Unknowns {
		if u.Kind != "user_only" || strings.TrimSpace(u.Question) == "" {
			continue
		}
		if !assumptions {
			b.WriteString("\n\n[No human available for these load-bearing decisions — make the recommended call" +
				" and state the assumption explicitly in the plan; do NOT re-raise them as open questions]:\n")
			assumptions = true
		}
		fmt.Fprintf(&b, "- %s → (no human available — make the recommended call and state the assumption)\n", u.Question)
	}
	var risks bool
	for _, u := range refl.Unknowns {
		if u.Kind != "non_blocking" || strings.TrimSpace(u.Question) == "" {
			continue
		}
		if !risks {
			b.WriteString("\n[Minor open questions to note under Risks — do NOT ask the user]:\n")
			risks = true
		}
		fmt.Fprintf(&b, "- %s\n", u.Question)
	}
	return b.String()
}

// synthesizePlan produces the final plan as a streamed call on the planner (reasoning)
// model, over the accumulated research history. Crucially it KEEPS ITS TOOLS: the planner
// often wants a final targeted read before it can write the plan, and a tool-less synthesis
// turned that need into a stall ("let me check one more thing" → short output → escalate to
// Opus, every time on a big task — you can't tell a model to plan while denying it the tools
// it says it needs). So this is a tool-enabled continuation: if the model does a final read,
// let it (execute + loop); when it returns prose with no tool call, that prose IS the plan.
// Bounded only by the loop's runaway guard (maxIterations) — no arbitrary synthesis cap.
func (s *Session) synthesizePlan(ctx context.Context, sys promptSpec, messages *[]wire.Message, committedOut, iterations int) (int, bool, error) {
	// Research is done — offload its stale raw tool-output to re-fetchable pointers before synthesis,
	// so the synthesis call (and every draft/review iteration that re-sends *messages) reasons over
	// the findings (in the prose) instead of dragging every grep/read/test dump from every round.
	s.offloadResearchForSynthesis(messages)
	// Initial draft: the planner writes the plan, free to take a final read OR ask a clarifying
	// question first (draftPlan keeps its tools). The reviewer-triggered revisions below reuse the
	// SAME primitive, so a revision can ALSO ask the user — not only auto-revise.
	resp, plan, err := s.draftPlan(ctx, sys, messages, &committedOut, &iterations, nil)
	if err != nil {
		if ctx.Err() != nil {
			s.printf("\n■ interrupted.\n")
			return iterations, false, nil
		}
		return iterations, false, err
	}
	// Backstop: if the model produced nothing usable even WITH tools (rare — genuine
	// runaway, or a truly hard plan), escalate synthesis to the strong model — the
	// legitimate court-of-appeal (plan_synth_incomplete is in lane.go's planEscalations).
	if len(plan) < minPlanLen && ctx.Err() == nil {
		s.printf("\n%s\n", metaStyle.Render("(plan incomplete — escalating to the strong model…)"))
		resp3, err3 := s.complete(ctx, llm.Synth, sys.withFact("nudge", "force").request(wire.Request{
			Messages: *messages, Tools: nil, Effort: wire.EffortHigh, MaxTokens: planMaxTokens,
			RoutingHint: &wire.RoutingHint{Reason: "plan_synth_incomplete"},
		}), committedOut, false)
		if err3 == nil {
			if t := strings.TrimSpace(resp3.Text()); len(t) > len(plan) {
				resp, plan = resp3, t
			}
		}
	}
	if plan != "" {
		s.lastText = plan
	}
	// Cross-model review: a 2nd cheap model (purpose=review) critiques the drafted plan before
	// it's presented. Skip when the plan was escalated to Opus (resp.Backend == "anthropic") —
	// Opus is the bar, a cheap critic would be noise. The review LOOPS, so a SECOND "revise"
	// actually triggers a second revision: ok → present as-is; revise → revise on the cheap
	// planner and RE-REVIEW; escalate → re-plan on the strong model and stop. A revise/escalate
	// verdict ALWAYS triggers a revision pass — even with no parseable findings (a generic
	// nudge) — never a silent "couldn't resolve". Bounded so review↔revise can't spin forever.
	const maxPlanReviewRounds = 3
	reviewUnresolved := false // a revision CALL produced nothing usable → plan stays flagged, not ready
	reviewed := false
	for round := 0; plan != "" && isCheapLane(resp.Backend) && ctx.Err() == nil && round < maxPlanReviewRounds; round++ {
		reviewed = true
		reviewMsgs := append(append([]wire.Message{}, *messages...), wire.Message{Role: "assistant", Blocks: resp.Blocks})
		// "Reviewing the plan…" is a PHASE (the review runs in a silenced forked sub-session),
		// emitted as muted status prose so the spinner doesn't look stuck on synthesis; the
		// durable record is the ⏺ ReviewPlan line below.
		s.printf("%s\n", metaStyle.Render("Reviewing the plan…"))
		v, reviewedBy := s.reviewWithTools(ctx, reviewMsgs)
		s.toolLine(true, "ReviewPlan", reviewedBy, v.Verdict, false) // ⏺ ReviewPlan(haiku…) · ok|revise|escalate
		if v.Verdict != "revise" && v.Verdict != "escalate" {
			break // ok (or any non-fix verdict) → present as-is
		}
		// ⎿ findings — the summary headline plus each concrete issue, one ⎿ line each.
		var findings []string
		for _, ln := range strings.Split(strings.TrimSpace(v.Summary), "\n") {
			if ln = strings.TrimSpace(ln); ln != "" {
				findings = append(findings, ln)
			}
		}
		for _, is := range v.Issues {
			if d := strings.TrimSpace(is.Detail); d != "" {
				findings = append(findings, d)
			}
		}
		for _, ln := range findings {
			s.printf("%s\n", metaStyle.Render("  ⎿ "+clip(ln, 300)))
		}
		// Build the revision nudge from everything the reviewer surfaced (findings + feedback).
		var detail strings.Builder
		for _, ln := range findings {
			detail.WriteString("- " + ln + "\n")
		}
		if fb := strings.TrimSpace(v.Feedback); fb != "" {
			detail.WriteString(fb)
		}
		body := strings.TrimSpace(detail.String())
		// revise = ALWAYS revise: the verdict alone triggers a revision pass; if the reviewer
		// left no parseable findings, nudge generically rather than presenting the flagged plan.
		escalate := v.Verdict == "escalate"
		nudge := "A reviewer flagged issues with this plan. Revise it to fix them, keeping its goal and approach intact:\n" + body
		if body == "" {
			nudge = "A reviewer judged this plan needs revision. Tighten it — close any gaps, ambiguity, or unjustified steps — keeping its goal and approach intact."
		}
		var hint *wire.RoutingHint
		if escalate { // load-bearing problem → re-plan on the strong model
			nudge = "A reviewer flagged a fundamental problem with this plan's approach. Re-plan to address it:\n" + body
			hint = &wire.RoutingHint{Reason: "plan_review_escalate"}
		}
		*messages = append(*messages, wire.Message{Role: "assistant", Blocks: resp.Blocks})
		*messages = append(*messages, wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: nudge}}})
		// Tool-enabled revision — the SAME draftPlan the initial synthesis uses, with the FULL
		// toolset. A reviewer "revise" isn't always an auto-fixable gap: when the concern needs
		// investigation (read the file the reviewer cited, grep, web-search) or a decision only the
		// USER can make (a contradicted design choice, an unverifiable assumption), the planner does
		// that work and folds it in — instead of a toothless one-shot regeneration that dead-ends.
		resp2, plan2, err2 := s.draftPlan(ctx, sys, messages, &committedOut, &iterations, hint)
		if err2 != nil || plan2 == "" {
			// The revision produced nothing usable (and didn't resolve via investigation/questions)
			// — keep the prior plan, flag it, stop.
			reviewUnresolved = true
			s.printf("\n%s\n", metaStyle.Render("(couldn't auto-resolve the reviewer's concerns above — tell me how you'd like to proceed, or revise the plan yourself)"))
			break
		}
		resp, plan = resp2, plan2
		s.lastText = plan
		if escalate {
			break // re-planned on Opus (the court of appeal) — don't cheap-review its output
		}
		// loop → re-review the revised plan (a second "revise" now triggers a second revision)
	}
	if reviewed {
		// The review (+ any revision) moved lastBackend/lastModel/lastPool — restore them to
		// the call that produced the SHOWN plan, so the footer/ServedBy stay honest.
		s.recordServed(func(v *servedState) {
			v.backend, v.model, v.pool, v.byok = resp.Backend, resp.Model, resp.Pool, resp.BYOK
		})
	}
	// Backstop: if even planMaxTokens wasn't enough, the plan is cut mid-sentence — say
	// so plainly instead of leaving a silently-truncated artifact (what "• **Adaptive-th"
	// looked like before the cap bump).
	if resp.StopReason == "max_tokens" {
		s.printf("\n%s\n", metaStyle.Render("(plan hit the output cap and may be truncated — /revise \"continue the plan\" to finish it)"))
	}
	*messages = append(*messages, wire.Message{Role: "assistant", Blocks: resp.Blocks})
	// A plan actually landed (not interrupted before synthesis) — Present moves the machine
	// to Presented so the TUI raises the approval selector. An empty synthesis, OR a
	// reviewer-flagged plan we couldn't revise, presents nothing (phase stays Researching)
	// so no "Execute" selector appears over a plan that isn't actually ready. The pin guard
	// (only a plan-SHAPED synthesis replaces the contract — the clobber bug) lives inside
	// Present; Effects.SavePlan fires the durable copy only when the pin was replaced.
	if plan != "" && !reviewUnresolved {
		eff, _ := s.planCtl.Present(plan)
		s.applyPlanEffects(ctx, eff)
	}
	return iterations, true, nil
}

// draftPlan runs the planner with its FULL toolset until it returns prose — that prose is the
// plan draft. Each iteration the model may take a final targeted read, investigate (grep,
// explore, web-search), OR ask the user a clarifying question (executeBatch routes ask_user
// through the HITL gate) before writing. It is the single primitive behind BOTH the initial
// synthesis and a reviewer-triggered revision, so a "revise" verdict that genuinely needs
// investigation or a user decision is resolved by doing that work — never neutered to a
// one-shot regeneration that dead-ends. committedOut and iterations advance in place (bounded
// by the loop's runaway guard); hint, when non-nil, forces a routing path (e.g. escalation).
// Returns the final response and its trimmed text ("" if the bound was hit before prose landed).
func (s *Session) draftPlan(ctx context.Context, sys promptSpec, messages *[]wire.Message, committedOut, iterations *int, hint *wire.RoutingHint) (wire.Response, string, error) {
	var resp wire.Response
	// The turn's own risk signal must ride the synthesis call too: without this default the
	// research loop escalates on high_risk_surface while the plan-WRITING call arrives at the
	// gateway risk-less and falls back to the cheap planner — the worst possible inversion
	// (the 2026-07-15 billing plan did exactly that). An explicit caller hint (escalation)
	// still wins. Side effect of drafting on the strong tier: the cheap cross-review below
	// self-skips (isCheapLane gate) — the strong draft IS the bar, per doctrine.
	if hint == nil {
		hint = s.turnRoutingHint()
	}
	for ; *iterations <= maxIterations; (*iterations)++ {
		req := wire.Request{Messages: *messages, Tools: s.toolDefs(), Effort: wire.EffortHigh, MaxTokens: planMaxTokens}
		if hint != nil {
			req.RoutingHint = hint
		}
		var err error
		resp, err = s.complete(ctx, llm.Synth, sys.withFact("nudge", "wrapup").request(req), *committedOut, false) // stream
		if err != nil {
			return resp, "", err
		}
		*committedOut += resp.OutputTokens
		uses := resp.ToolUses()
		if len(uses) == 0 {
			return resp, strings.TrimSpace(resp.Text()), nil // prose, no tool call → this is the plan
		}
		// A final read, an investigation, or a clarifying ask_user — execute and loop rather than
		// forcing a stall (the model said it needs this before it can write/revise the plan).
		*messages = append(*messages, wire.Message{Role: "assistant", Blocks: resp.Blocks})
		results := s.executeBatchHooked(ctx, uses)
		*messages = append(*messages, wire.Message{Role: "user", Blocks: results})
		s.recordTurn(resp.Text(), uses)
	}
	return resp, strings.TrimSpace(resp.Text()), nil
}

// complete runs one model call THROUGH THE METERED GATEWAY (s.runner), so token +
// cost accounting is automatic — no per-call accrue, no sub-agent rollup. The
// Runner streams if the provider can (live deltas + a climbing ↓ token estimate,
// snapped to the authoritative count when usage arrives), else falls back to a
// single Complete; either way it records usage to the shared Ledger under purpose.
// committedOut is the output-token total already streamed this turn (so ↓ keeps
// climbing across tool iterations); suppressText hides streamed prose (plan-mode
// research turns) while still metering. purpose attributes the spend (main_loop /
// reflect / synth) in the ledger.
func (s *Session) complete(ctx context.Context, purpose llm.Purpose, req wire.Request, committedOut int, suppressText bool) (wire.Response, error) {
	// memcode does NOT stream model output. It's an agentic CLI — most turns are tool calls,
	// not prose, so live token-by-token display is low value; and streaming GLM-family tool
	// calls trips an upstream streaming tool-call PARSER bug (vLLM #42400/#39757/#36857, worse
	// near the context limit) that leaks the call as TEXT and dead-ends the turn. One COMPLETE
	// response → the whole tool call is assembled server-side (no chunk-boundary truncation),
	// and scrollback rendering stays simple (only ever whole lines, never a partial delta).
	resp, err := s.runner.Complete(ctx, purpose, req)
	s.traceWire(purpose, req, resp, err) // env-gated (MEMCODE_TRACE) end-to-end wire capture
	if err == nil {
		s.notifyTokens(committedOut + resp.OutputTokens) // snap the ↓ estimate to the real count
		if !suppressText {
			if txt := strings.TrimSpace(resp.Text()); txt != "" {
				s.printf("\n%s\n\n", txt) // the reply as ONE block (→ absorb → wrapScrollback)
			}
		}
	}
	// Backend visibility: the gateway tags every response with who served it
	// (cheap | anthropic | …) + the model. Surface it after EVERY reply, every build — it
	// was dev-only AND only-on-change, so a steady session never showed who served it,
	// which is exactly the routing signal you want to see. AFTER the reply's terminator
	// so it never glues onto streamed text. Glyph ⇄ (a routing line), NOT ● — the TUI
	// rolls standalone ● lines into the spinner (isNoise), which once ate this line.
	if err == nil && resp.Backend != "" {
		s.recordServed(func(v *servedState) {
			v.backend = resp.Backend
			v.pool = resp.Pool               // the cheap-lane model's short name (e.g. glm-5p1); "" on Anthropic
			v.model = resp.Model             // the footer should show what ACTUALLY served, not the session default
			v.byok = resp.BYOK               // UNCONDITIONAL: a non-BYOK turn clears the footer's byok segment — nothing sticky
			v.ctxWindow = resp.ContextWindow // real serving window (cheap lane); 0 on Anthropic ⇒ meter uses model default
			// Learn the input budget (smallest seen) from the session's HOME lane only:
			// the cheap lane under Automatic, or whatever lane a /model pin rides. An
			// Automatic escalation must NOT teach the budget — a grok absorb's 460K
			// would permanently throttle a session whose everyday lane fits 960K
			// (min-seen never rises; SetPin resets it when the home lane changes).
			if resp.InputBudget > 0 && resp.FallbackReason == "" &&
				(isCheapLane(resp.Backend) || s.pin != "") &&
				(v.inputBudget == 0 || resp.InputBudget < v.inputBudget) {
				v.inputBudget = resp.InputBudget
			}
		})
		// Just the model name — it already implies the backend (kimi-k2p6 = cheap lane, opus =
		// Anthropic). The cheap lane tags a short Pool label; Anthropic has none, so fall back to
		// the short model id.
		name := resp.Pool
		if name == "" {
			name = provider.ShortModel(resp.Model)
		}
		line := "⇄ served by " + name
		// Why this turn isn't on the pool — the dogfooding instrument for the 80/20 flip.
		// "thinking"/"planner"/"vision" = deterministic policy; "escalate:<reason>" = a
		// routing_hint; "cheap_lane_error"/"cheap_lane_overflow"/… = a real fallback. A drift back toward
		// Anthropic shows up here turn-by-turn (and aggregated in /cost by-backend).
		if resp.FallbackReason != "" {
			line += " · " + resp.FallbackReason
		}
		// The CLI's own escalation HINT for this turn (high_risk_surface / user_friction_elevated
		// / user_friction_high / room_repair / self_heal / plan_review_escalate), surfaced so an
		// unexpected escalation is legible —
		// e.g. a read-only plan that merely MENTIONS "rm -rf" trips high_risk_surface. The
		// gateway weighs the hint; in plan mode it ignores mood/friction hints but DOES escalate
		// the executive on high_risk_surface and on the reviewer's verdict (resolve.go), so this
		// is the thing to read when a plan turn lands somewhere you didn't expect.
		// Room/friction hints (user_friction_*) are internal interaction state — not surfaced
		// (Tim: "don't show the room stuff"). Genuine routing reasons (self_heal, escalate,
		// high_risk_surface) STILL show, so an unexpected escalation stays legible.
		if req.RoutingHint != nil && req.RoutingHint.Reason != "" && !strings.HasPrefix(req.RoutingHint.Reason, "user_friction") {
			line += " · hint:" + req.RoutingHint.Reason
		}
		// Thinking EFFORT (also shown live on the spinner): the depth this turn reasoned at.
		if req.Effort != wire.EffortOff {
			line += " · " + string(req.Effort) + " effort"
		}
		// One line per turn: a turn makes several model calls (tool loop, hidden recall,
		// final answer) — print it once and re-announce only when routing actually shifts
		// mid-turn (e.g. a cheap-lane gather that absorbs to the strong tier). s.turn.servedLine is reset
		// at the top of each user turn (runLoop), so every turn still re-announces.
		if line != s.turn.servedLine {
			s.turn.servedLine = line
			// Interactive chat wants the trailing blank before the next prompt;
			// headless Run() (liveChat == nil) is one-shot, so no trailing blank.
			if s.liveChat == nil {
				s.printf("%s\n", metaStyle.Render(line))
			} else {
				s.printf("%s\n\n", metaStyle.Render(line))
			}
		}
	}
	if err == nil {
		s.captureToolCallLeak(ctx, purpose, req, resp)
	}
	return resp, err
}

// leaksToolCallText reports whether model output contains MiniMax's tool-call special-token
// envelope leaking as plain text (the recurring M-series quirk). Keyed on the minimax marker
// specifically (NOT a bare <invoke>/<tool_call>, which appear legitimately in memcode's own
// source) so it doesn't false-positive while editing this repo.
func leaksToolCallText(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "minimax[>") || strings.Contains(l, "<minimax:tool_call")
}

// nonEmptyAssistant guarantees an assistant message has renderable content: an empty
// or all-blank-text block set (the stall / empty-turn case) is replaced with a single
// placeholder text block. A message with no content is rejected by the provider API on
// the NEXT turn ("all messages must have non-empty content"), so an empty assistant
// turn left in history would 400 the whole rest of the conversation.
func nonEmptyAssistant(blocks []wire.Block) []wire.Block {
	for _, b := range blocks {
		if b.Type != "text" || strings.TrimSpace(b.Text) != "" {
			return blocks // has a tool_use/thinking block or real text
		}
	}
	return []wire.Block{{Type: "text", Text: "(no output)"}}
}

// captureToolCallLeak writes a LOCAL diagnostic when a response leaks tool-call markup as text.
// It records the EXACT request we sent — the full message history, mode, facts, purpose, tools
// offered — next to the leaked output, so we can inspect whether the trigger is OUR request
// shape or the model, instead of guessing. Local file (.memcode/diagnostics/) so it's always
// findable with no cloud/log-size limits; best-effort, never blocks a turn.
func (s *Session) captureToolCallLeak(ctx context.Context, purpose llm.Purpose, req wire.Request, resp wire.Response) {
	text := resp.Text()
	if !leaksToolCallText(text) {
		return
	}
	rec := map[string]any{
		"at":            time.Now().UTC().Format(time.RFC3339Nano),
		"served_model":  resp.Model,
		"backend":       resp.Backend,
		"purpose":       string(purpose),
		"mode":          req.Mode,
		"effort":        string(req.Effort),
		"facts":         req.Facts,
		"tools_offered": len(req.Tools),
		"messages":      req.Messages, // the FULL conversation history we sent — the thing to inspect
		"leaked_output": text,
	}
	dir := filepath.Join(s.root, ".memcode", "diagnostics")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("toolcall-leak-%d.json", time.Now().UnixNano()))
	if os.WriteFile(path, b, 0o644) != nil {
		return
	}
	s.emit(ctx, events.KindNote, map[string]any{"note": "toolcall_leak_captured", "path": path})
	s.printf("%s\n", metaStyle.Render("  ⚠ captured a tool-call leak → "+path))
}

func (s *Session) notifyTokens(out int) {
	if s.observer != nil {
		s.observer.Tokens(out)
	}
}

// estimateTokens is a rough chars→tokens heuristic for the live counter (~4
// chars/token for English/code); the real count replaces it when usage arrives.
func estimateTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return chars / 4
}

// turnBillingLane returns the billing-lane extension for this turn's model
// calls: "credits" only after the user explicitly consented to serve this
// turn on memcode credits (a BYOK key failure); "" otherwise (byok-preferred,
// the default the gateway enforces).
func (s *Session) turnBillingLane() string {
	if s.turn.billingCredits {
		return "credits"
	}
	return ""
}

// consentCreditsRetry asks the user — main interactive sessions only —
// whether to serve the CURRENT turn on memcode credits after their own
// provider key failed. Consent is per-turn and explicit: the gateway can
// never reroute between the user's keys and credits on its own, and neither
// can this loop without a yes.
func (s *Session) consentCreditsRetry(ctx context.Context) bool {
	if s.ask == nil || s.purpose != llm.MainLoop || s.turn.billingCredits {
		return false // headless / sub-agent / already consented-and-failed-again
	}
	resp := s.ask(ctx, AskRequest{
		Question: "Your API key was rejected for this turn. Retry this turn on memcode credits instead?",
		Options: []AskOption{
			{Label: "Retry on credits", Description: "This turn only — billed to your memcode credit balance"},
			{Label: "Stop the turn", Description: "Fix or remove the key with /apikeys first"},
		},
	})
	ans := strings.ToLower(strings.TrimSpace(resp.Answer))
	if strings.HasPrefix(ans, "retry") || ans == "y" || ans == "yes" {
		s.turn.billingCredits = true
		return true
	}
	return false
}
