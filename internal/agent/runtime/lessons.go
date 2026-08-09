package runtime

// Distilled-failure memory, runtime side. When a turn breaks its own edits and
// repairs them (the completion-gate self-heal path), the failure AND the fix
// are both known — the one moment a strategy-level lesson can be distilled.
// The distillation is one small model call (mode "distill", purpose learn);
// the signal dual-writes like preference_signal (SQLite event = derived index,
// events.jsonl = canonical), accumulates under the same promotion rigor
// (≥3 signals, ≥2 sessions, weighted ≥2.0 — lessons must RECUR to bind), and
// promoted lessons surface at session start as ranked background DATA.

import (
	"context"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/lessons"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"

	"github.com/memcode-ai/memcode/internal/wire"
)

const (
	distillTimeout     = 30 * time.Second
	lessonStrength     = 0.6 // system-observed evidence: above a drive-by mention, below a user directive
	maxLessonTrigger   = 160
	maxLessonStrategy  = 200
	maxDistillEvidence = 1600 // clip the failure text fed to the distiller
)

// distillLesson extracts a strategy lesson from this turn's failure-and-repair
// episode, asynchronously — the finishing turn never waits on it. Fire-and-
// forget: a failed distillation just means no signal.
func (s *Session) distillLesson(finalText string) {
	if s.runner == nil || strings.TrimSpace(s.turn.firstBreak) == "" {
		return
	}
	var edited []string
	for p := range s.turn.editedPaths {
		edited = append(edited, p)
	}
	failure := s.turn.firstBreak
	bg := s.bgCtx
	if bg == nil {
		bg = context.Background()
	}
	go func() {
		ctx, cancel := context.WithTimeout(bg, distillTimeout)
		defer cancel()
		var b strings.Builder
		b.WriteString("episode: failure-and-repair\n\n")
		b.WriteString("Failure the agent caused while editing:\n")
		b.WriteString(truncate(failure, maxDistillEvidence))
		b.WriteString("\n\nFiles involved: " + strings.Join(edited, ", "))
		b.WriteString("\n\nThe agent's account of the fix:\n")
		b.WriteString(truncate(strings.TrimSpace(finalText), maxDistillEvidence))
		trigger, strategy, ok := s.distill(ctx, b.String())
		if !ok {
			return
		}
		s.emitLessonSignalFor(ctx, trigger, strategy, s.sessionID, s.headSHA, editedList(s.turn.editedPaths))
	}()
}

// distill runs one distillation model call over an episode's evidence and
// parses the TRIGGER/STRATEGY contract. ok=false on error, the "none" sentinel,
// or a malformed reply — a lost episode is dropped, never retried.
func (s *Session) distill(ctx context.Context, evidence string) (trigger, strategy string, ok bool) {
	resp, err := s.sideComplete(ctx, llm.Learn, wire.Request{
		Mode:      "distill",
		Messages:  []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: s.redactor.Redact(evidence)}}}},
		MaxTokens: 300,
	})
	if err != nil {
		return "", "", false
	}
	return parseLessonReply(resp.Text())
}

// parseLessonReply extracts the two-line TRIGGER/STRATEGY contract; ok=false on
// the "none" sentinel or any malformed reply (a bad distillation is dropped,
// never stored).
func parseLessonReply(text string) (trigger, strategy string, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if rest, found := strings.CutPrefix(line, "TRIGGER:"); found {
			trigger = strings.TrimSpace(rest)
		} else if rest, found := strings.CutPrefix(line, "STRATEGY:"); found {
			strategy = strings.TrimSpace(rest)
		}
	}
	if trigger == "" || strategy == "" || strings.EqualFold(trigger, "none") {
		return "", "", false
	}
	return clip(trigger, maxLessonTrigger), clip(strategy, maxLessonStrategy), true
}

// emitLessonSignalFor dual-writes a lesson signal: SQLite event (the reducer's
// derived index) + the canonical session-log record (survives a state.db
// rebuild). sessionID/headSHA are the episode's provenance — for the live-turn
// distiller that's the current session; for the post-session loop it's the
// TARGET session the outcome was about (the record still lands in the current
// session's log, tagged with TargetSession).
func (s *Session) emitLessonSignalFor(ctx context.Context, trigger, strategy, sessionID, headSHA string, files []string) {
	trigger = s.redactor.Redact(trigger)
	strategy = s.redactor.Redact(strategy)
	payload := map[string]any{
		"trigger": trigger, "strategy": strategy, "strength": lessonStrength,
		"files": files, "head_sha": headSHA,
	}
	if sessionID != s.sessionID {
		// emit stamps session_id with the CURRENT session; the episode's true
		// session travels as target_session (the reducer prefers it).
		payload["target_session"] = sessionID
	}
	s.emit(ctx, events.KindLessonSignal, payload)
	rec := sessionlog.Record{
		Kind: sessionlog.KindLessonSignal, Trigger: trigger, Strategy: strategy, Strength: lessonStrength,
		HeadSHA: headSHA,
	}
	if sessionID != s.sessionID {
		rec.TargetSession = sessionID
	}
	s.slog.Append(rec)
}

func editedList(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	return out
}

// applyLessonPromotions mirrors applyPreferencePromotions: silent, deterministic,
// run from StartChat. File existence is the promotion status — no candidate
// table; deleting .memcode/lessons/<id>-*.md revokes a lesson for good only
// until it re-earns the bar (same contract as prefs demotion-by-edit).
func (s *Session) applyLessonPromotions(ctx context.Context) {
	s.backfillLessonSignals(ctx)
	cands, err := lessons.Reduce(ctx, s.store)
	if err != nil {
		return
	}
	for _, c := range lessons.PendingPromotions(s.root, cands) {
		path, err := lessons.WriteLesson(s.root, c)
		if err != nil {
			continue
		}
		s.emit(ctx, events.KindDecision, map[string]any{
			"kind": "lesson_promoted", "trigger": c.Trigger, "strategy": c.Strategy, "path": path,
		})
		s.printf("  ℹ promoted failure lesson: %s\n", clip(c.Trigger, 80))
	}
	// Demote lessons the adherence loop has falsified: the agent kept violating
	// the rule on sessions the human then corrected/rejected. Marker-backed so
	// the same stale evidence can't re-promote next startup (see lessons.Demote).
	for _, c := range lessons.PendingDemotions(s.root, cands) {
		if err := lessons.Demote(s.root, c); err != nil {
			continue
		}
		s.emit(ctx, events.KindDecision, map[string]any{
			"kind": "lesson_demoted", "trigger": c.Trigger, "strategy": c.Strategy, "violations": c.Violations,
		})
		s.printf("  ℹ demoted lesson (violated %d× on corrected/rejected work): %s\n", c.Violations, clip(c.Trigger, 70))
	}
}

// backfillLessonSignals recovers canonical signals from events.jsonl into the
// SQLite index after a state.db wipe. Steady-state no-op (same guard as prefs).
func (s *Session) backfillLessonSignals(ctx context.Context) {
	existing, err := s.store.ListEvents(ctx, store.EventFilter{
		Kinds: []string{string(events.KindLessonSignal)}, Limit: 1,
	})
	if err != nil || len(existing) > 0 {
		return
	}
	recs, err := sessionlog.LessonSignals(s.root)
	if err != nil || len(recs) == 0 {
		return
	}
	failed := 0
	for _, r := range recs {
		sid := r.SessionID
		if r.TargetSession != "" {
			sid = r.TargetSession // the episode's session, not the session that judged it
		}
		if _, err := s.store.AppendEvent(ctx, store.Event{
			TS:   r.TS,
			Kind: string(events.KindLessonSignal),
			Payload: mustJSON(map[string]any{
				"trigger": r.Trigger, "strategy": r.Strategy,
				"strength": r.Strength, "session_id": sid, "head_sha": r.HeadSHA,
			}),
		}); err != nil {
			failed++
		}
	}
	if failed > 0 {
		s.printf("⚠ %d/%d lesson signals couldn't be recovered\n", failed, len(recs))
	}
}

// inlineLessons is the surfacing layer: top promoted lessons as a compact
// context block (data framing, redacted).
func (s *Session) inlineLessons() string {
	return s.redactor.Redact(lessons.Inline(s.root))
}

// inlineLessonsTop is inlineLessons plus the surfaced lessons' stable ids —
// recorded as context_inlined for the adherence judge.
func (s *Session) inlineLessonsTop() (string, []string) {
	block, ids := lessons.InlineTop(s.root)
	return s.redactor.Redact(block), ids
}
