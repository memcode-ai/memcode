package runtime

// The post-session learning loop — the "sleep consolidation" step of memcode's
// memory. Acceptance (acceptance.Reconcile, run at startup) has already read
// git and recorded each finished session's fate as a session_outcome event.
// This file turns those outcomes into learning:
//
//	corrected/rejected → distill a lesson from the human's diff (what the
//	                     human changed IS the correction; episode kinds
//	                     human-correction / rejection)
//	rules in context   → judge adherence (mode "adhere", cheap classifier
//	                     lane): violated on corrected/rejected work demotes
//	                     the rule, followed on accepted work reinforces it
//
// Every outcome is processed at most once (outcome_processed marker — an
// attempt counts; a failed gateway call drops the episode, never retries).
// Bounded per startup, async, fire-and-forget: session start never waits.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/lessons"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"

	"github.com/memcode-ai/memcode/internal/wire"
)

const (
	maxOutcomesPerStart = 5    // cost bound: at most 5 outcome learning passes per startup
	maxDigestChars      = 2400 // adherence session digest clip
	maxRuleChars        = 220  // one rule line in the adherence prompt
	adhereTimeout       = 30 * time.Second
)

// outcomeSession is everything the learning pass knows about one finished session.
type outcomeSession struct {
	id        string
	outcome   string // accepted | corrected | rejected
	evidence  string // acceptance's evidence string ("2/3 agent file(s) …")
	baseline  string // head_sha at session start
	files     []string
	lessonIDs []string // rules that rode this session's prompt
	prefIDs   []string
	processed bool
	ts        time.Time
}

// processOutcomes scans for finished sessions whose git fate is known but whose
// learning pass hasn't run, and runs it. Called async from StartChat.
func (s *Session) processOutcomes(ctx context.Context) {
	if s.runner == nil {
		return
	}
	targets := s.unprocessedOutcomes(ctx)
	for _, t := range targets {
		// Marker FIRST-ish semantics via defer would race a crash into a retry
		// loop; marker after attempts, and an attempt that dies mid-way simply
		// re-runs once more next startup — both calls are idempotent signals.
		if t.outcome == "corrected" || t.outcome == "rejected" {
			s.distillFromOutcome(ctx, t)
		}
		if len(t.lessonIDs) > 0 || len(t.prefIDs) > 0 {
			s.judgeAdherence(ctx, t)
		}
		s.factsFromSession(ctx, t)
		// target_session, NOT session_id: s.emit stamps session_id with the
		// CURRENT session unconditionally (runtime.go emit), which would mark
		// the wrong session forever (found live: the marker landed on the
		// judging session and the corrected one stayed eligible).
		s.emit(ctx, events.KindOutcomeProcessed, map[string]any{"target_session": t.id})
	}
}

// unprocessedOutcomes assembles outcome sessions from the event log, oldest
// first, capped at maxOutcomesPerStart.
func (s *Session) unprocessedOutcomes(ctx context.Context) []outcomeSession {
	evs, err := s.store.ListEvents(ctx, store.EventFilter{Kinds: []string{
		string(events.KindSessionOutcome),
		string(events.KindOutcomeProcessed),
		string(events.KindContextInlined),
		string(events.KindAgentSessionStarted),
		string(events.KindFileEdited),
	}})
	if err != nil {
		return nil
	}
	sessions := map[string]*outcomeSession{}
	get := func(id string) *outcomeSession {
		if sessions[id] == nil {
			sessions[id] = &outcomeSession{id: id}
		}
		return sessions[id]
	}
	seenFile := map[string]map[string]bool{}
	for _, e := range evs {
		var p struct {
			SessionID     string   `json:"session_id"`
			TargetSession string   `json:"target_session"`
			Outcome       string   `json:"outcome"`
			Evidence      string   `json:"evidence"`
			HeadSHA       string   `json:"head_sha"`
			Path          string   `json:"path"`
			LessonIDs     []string `json:"lesson_ids"`
			PrefIDs       []string `json:"pref_ids"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		// The marker names its target in target_session (session_id is the
		// judging session, stamped by emit). Everything else keys on session_id.
		key := p.SessionID
		if events.Kind(e.Kind) == events.KindOutcomeProcessed {
			key = p.TargetSession
		}
		if key == "" {
			continue
		}
		t := get(key)
		switch events.Kind(e.Kind) {
		case events.KindSessionOutcome:
			t.outcome, t.evidence, t.ts = p.Outcome, p.Evidence, e.TS
		case events.KindOutcomeProcessed:
			t.processed = true
		case events.KindContextInlined:
			t.lessonIDs, t.prefIDs = p.LessonIDs, p.PrefIDs
		case events.KindAgentSessionStarted:
			t.baseline = p.HeadSHA
		case events.KindFileEdited:
			if p.Path == "" {
				continue
			}
			if seenFile[p.SessionID] == nil {
				seenFile[p.SessionID] = map[string]bool{}
			}
			if !seenFile[p.SessionID][p.Path] {
				seenFile[p.SessionID][p.Path] = true
				t.files = append(t.files, p.Path)
			}
		}
	}
	var out []outcomeSession
	for _, t := range sessions {
		if t.outcome == "" || t.processed || t.id == s.sessionID {
			continue
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ts.Before(out[j].ts) })
	if len(out) > maxOutcomesPerStart {
		out = out[:maxOutcomesPerStart]
	}
	return out
}

// distillFromOutcome distills a lesson from a corrected/rejected session: the
// human's diff against the session baseline is the evidence — what the human
// changed is the correction.
func (s *Session) distillFromOutcome(ctx context.Context, t outcomeSession) {
	if t.baseline == "" || len(t.files) == 0 {
		return // no baseline to diff against — nothing trustworthy to learn from
	}
	diff := s.gitDiffFiles(ctx, t.baseline, t.files)
	if strings.TrimSpace(diff) == "" && t.outcome == "corrected" {
		return // corrected but no visible delta (e.g. history rewritten) — skip
	}
	episode := "human-correction"
	if t.outcome == "rejected" {
		episode = "rejection"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "episode: %s\n\n", episode)
	if task := firstUserMessage(s.root, t.id); task != "" {
		b.WriteString("The task the agent was doing:\n" + truncate(task, 300) + "\n\n")
	}
	fmt.Fprintf(&b, "Git verdict on the agent's work: %s (%s)\n", t.outcome, t.evidence)
	b.WriteString("Files involved: " + strings.Join(t.files, ", ") + "\n\n")
	if t.outcome == "rejected" {
		b.WriteString("The agent's patch was reverted or discarded. Diff of what remains vs the agent's baseline:\n")
	} else {
		b.WriteString("What the human changed after the agent (diff vs the session baseline):\n")
	}
	b.WriteString(truncate(diff, maxDistillEvidence))
	ctx, cancel := context.WithTimeout(ctx, distillTimeout)
	defer cancel()
	trigger, strategy, ok := s.distill(ctx, b.String())
	if !ok {
		return
	}
	s.emitLessonSignalFor(ctx, trigger, strategy, t.id, t.baseline, t.files)
}

// judgeAdherence asks the cheap classifier lane whether the rules that rode a
// finished session's prompt were followed, and records the verdicts. Verdicts
// feed the reducers' weights (violations demote, followed+accepted reinforces).
func (s *Session) judgeAdherence(ctx context.Context, t outcomeSession) {
	rules := s.ruleLines(t)
	if len(rules) == 0 {
		return // every inlined rule has since been demoted/deleted
	}
	digest := s.sessionDigest(t)
	if digest == "" {
		return
	}
	var b strings.Builder
	b.WriteString("Rules that were in the agent's context:\n")
	ids := make([]string, 0, len(rules))
	for id := range rules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		fmt.Fprintf(&b, "%d. id=%s: %s\n", i+1, id, clip(rules[id], maxRuleChars))
	}
	b.WriteString("\nSession digest:\n" + digest)
	cctx, cancel := context.WithTimeout(ctx, adhereTimeout)
	defer cancel()
	// Forced tool (the judge pattern): the classify lane serves reasoning
	// models that ramble before prose JSON; a forced call can't. MaxTokens 0 =
	// uncapped per the judges-uncapped ruling — a fixed cap truncated verdicts
	// mid-object when the model spent tokens thinking first.
	resp, err := s.sideComplete(cctx, llm.Classify, wire.Request{
		Mode:       "adhere",
		Messages:   []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: s.redactor.Redact(b.String())}}}},
		Tools:      []wire.ToolDef{adherenceTool},
		ToolChoice: adherenceTool.Name,
	})
	if err != nil {
		return
	}
	for _, v := range decodeAdherence(resp) {
		if rules[v.ID] == "" || (v.Verdict != "followed" && v.Verdict != "violated") {
			continue // unknown id or not_applicable — nothing to record
		}
		kind := "pref"
		if strings.HasPrefix(v.ID, "lesson_") {
			kind = "lesson"
		}
		s.emit(cctx, events.KindAdherence, map[string]any{
			"target_session": t.id, "rule_kind": kind, "rule_id": v.ID,
			"verdict": v.Verdict, "outcome": t.outcome,
		})
		s.slog.Append(sessionlog.Record{
			Kind: sessionlog.KindAdherence, RuleKind: kind, RuleID: v.ID,
			Verdict: v.Verdict, Outcome: t.outcome, TargetSession: t.id,
		})
	}
}

const (
	factsTimeout     = 45 * time.Second
	maxFactsEvidence = 24_000 // transcript chars fed to the facts extractor
	maxFactsPerRun   = 20
)

// factsFromSession extracts atomic facts from a settled session's transcript
// (one gateway "facts" call) and appends them to THAT session's log as
// KindFacts records, where ranked search indexes them and the entity graph
// reads them. Idempotent: a session that already carries facts is skipped.
func (s *Session) factsFromSession(ctx context.Context, t outcomeSession) {
	recs, err := sessionlog.Recent(s.root, t.id, 0)
	if err != nil || len(recs) == 0 {
		return
	}
	var b strings.Builder
	for _, r := range recs {
		switch r.Kind {
		case sessionlog.KindFacts:
			return // already extracted
		case sessionlog.KindUserMessage:
			b.WriteString("USER: " + r.Text + "\n")
		case sessionlog.KindAssistantMessage:
			b.WriteString("ASSISTANT: " + r.Text + "\n")
		case sessionlog.KindCompaction:
			b.WriteString("SUMMARY: " + r.Text + "\n")
		}
	}
	transcript := strings.TrimSpace(b.String())
	if len(transcript) < 200 {
		return // too little dialogue to yield durable facts
	}
	cctx, cancel := context.WithTimeout(ctx, factsTimeout)
	defer cancel()
	// Forced tool: the standard lane serves reasoning models — prose JSON
	// rambles, a forced call can't. Uncapped output per the judges ruling.
	resp, err := s.sideComplete(cctx, llm.Learn, wire.Request{
		Mode:       "facts",
		Messages:   []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: s.redactor.Redact(truncate(transcript, maxFactsEvidence))}}}},
		Tools:      []wire.ToolDef{factsTool},
		ToolChoice: factsTool.Name,
	})
	if err != nil {
		return
	}
	facts := decodeFacts(resp)
	if len(facts) == 0 {
		return
	}
	// Appending to an old session's log would bump its mtime and corrupt
	// recency ordering (RecentSessions and burst grouping are mtime-based) —
	// capture and restore it around the write.
	evPath := filepath.Join(s.root, config.DirName, "sessions", t.id, "events.jsonl")
	st, statErr := os.Stat(evPath)
	w, err := sessionlog.Open(s.root, t.id)
	if err != nil {
		return
	}
	for _, f := range facts {
		w.Append(sessionlog.Record{Kind: sessionlog.KindFacts, Text: f.Fact, Entities: f.Entities})
	}
	_ = w.Close()
	if statErr == nil {
		_ = os.Chtimes(evPath, st.ModTime(), st.ModTime())
	}
}

type extractedFact struct {
	Fact     string   `json:"fact"`
	Entities []string `json:"entities"`
}

// factsTool is the forced structured-output contract for fact extraction.
var factsTool = wire.ToolDef{
	Name:        "record_facts",
	Description: "Record the durable facts extracted from the session transcript.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"facts": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"fact":     map[string]any{"type": "string"},
						"entities": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
					"required": []string{"fact"},
				},
			},
		},
		"required": []string{"facts"},
	},
}

// decodeFacts decodes the forced tool call, falling back to the legacy
// prose-array parse, then normalizes (trim, lowercase entities, cap count).
func decodeFacts(resp wire.Response) []extractedFact {
	var p struct {
		Facts []extractedFact `json:"facts"`
	}
	if decodeForcedTool(resp, factsTool, &p) && len(p.Facts) > 0 {
		return normalizeFacts(p.Facts)
	}
	return parseFactsReply(resp.Text())
}

// parseFactsReply parses the facts mode's legacy strict-JSON-array contract,
// tolerating code fences. Malformed replies drop silently — a bad extraction
// must never poison the log. Fallback only — the primary contract is the
// forced factsTool call.
func parseFactsReply(text string) []extractedFact {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "["); i >= 0 {
		if j := strings.LastIndex(text, "]"); j > i {
			text = text[i : j+1]
		}
	}
	var facts []extractedFact
	if json.Unmarshal([]byte(text), &facts) != nil {
		return nil
	}
	return normalizeFacts(facts)
}

// normalizeFacts trims, lowercases entities, and caps the per-run count.
func normalizeFacts(facts []extractedFact) []extractedFact {
	out := facts[:0]
	for _, f := range facts {
		f.Fact = strings.TrimSpace(f.Fact)
		if f.Fact == "" {
			continue
		}
		for i, e := range f.Entities {
			f.Entities[i] = strings.ToLower(strings.TrimSpace(e))
		}
		out = append(out, f)
		if len(out) >= maxFactsPerRun {
			break
		}
	}
	return out
}

// ruleLines resolves the inlined rule ids to their current text. A rule deleted
// or demoted since the session simply drops out.
func (s *Session) ruleLines(t outcomeSession) map[string]string {
	out := map[string]string{}
	if len(t.lessonIDs) > 0 {
		want := map[string]bool{}
		for _, id := range t.lessonIDs {
			want[id] = true
		}
		for _, l := range lessons.List(s.root) {
			if want[l.ID] {
				out[l.ID] = "when " + l.Trigger + " → " + l.Strategy
			}
		}
	}
	if len(t.prefIDs) > 0 {
		want := map[string]bool{}
		for _, id := range t.prefIDs {
			want[id] = true
		}
		dir := filepath.Join(s.root, ".memcode", "prefs")
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			id := prefIDFromFilename(e.Name())
			if !want[id] {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			if p := parsePrefFile(string(data)); p.text != "" {
				out[id] = "[" + p.axis + "] " + p.text
			}
		}
	}
	return out
}

// sessionDigest renders a compact, redacted trail of the target session: what
// the user asked, what the agent last said, what was edited, how git judged it.
func (s *Session) sessionDigest(t outcomeSession) string {
	recs, err := sessionlog.SessionRecords(s.root, t.id)
	if err != nil || len(recs) == 0 {
		return ""
	}
	var users []string
	var lastAssistant string
	for _, r := range recs {
		switch r.Kind {
		case sessionlog.KindUserMessage:
			if len(users) < 3 && strings.TrimSpace(r.Text) != "" {
				users = append(users, truncate(strings.TrimSpace(r.Text), 240))
			}
		case sessionlog.KindAssistantMessage:
			if strings.TrimSpace(r.Text) != "" {
				lastAssistant = strings.TrimSpace(r.Text)
			}
		}
	}
	var b strings.Builder
	for _, u := range users {
		b.WriteString("User asked: " + u + "\n")
	}
	if lastAssistant != "" {
		b.WriteString("Agent's final message: " + truncate(lastAssistant, 400) + "\n")
	}
	if len(t.files) > 0 {
		b.WriteString("Files the agent edited: " + strings.Join(t.files, ", ") + "\n")
	}
	fmt.Fprintf(&b, "Git verdict afterward: %s (%s)\n", t.outcome, t.evidence)
	return s.redactor.Redact(truncate(b.String(), maxDigestChars))
}

// gitDiffFiles diffs the worktree against baseline for the given paths —
// committed and uncommitted human changes alike.
func (s *Session) gitDiffFiles(ctx context.Context, baseline string, files []string) string {
	args := append([]string{"-C", s.root, "diff", baseline, "--"}, files...)
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// firstUserMessage returns the target session's opening request, if logged.
func firstUserMessage(root, id string) string {
	recs, err := sessionlog.SessionRecords(root, id)
	if err != nil {
		return ""
	}
	for _, r := range recs {
		if r.Kind == sessionlog.KindUserMessage && strings.TrimSpace(r.Text) != "" {
			return strings.TrimSpace(r.Text)
		}
	}
	return ""
}

// adherenceVerdict is one parsed judge verdict.
type adherenceVerdict struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
}

// adherenceTool is the forced structured-output contract for the adherence
// judge.
var adherenceTool = wire.ToolDef{
	Name:        "record_adherence",
	Description: "Record, for each rule id, whether the session followed it.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"verdicts": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":      map[string]any{"type": "string"},
						"verdict": map[string]any{"type": "string", "enum": []string{"followed", "violated", "not_applicable"}},
					},
					"required": []string{"id", "verdict"},
				},
			},
		},
		"required": []string{"verdicts"},
	},
}

// decodeAdherence decodes the forced tool call (prose-JSON fallback included
// via decodeForcedTool, then the legacy brace-scrape for maximum tolerance).
func decodeAdherence(resp wire.Response) []adherenceVerdict {
	var p struct {
		Verdicts []adherenceVerdict `json:"verdicts"`
	}
	if decodeForcedTool(resp, adherenceTool, &p) {
		return p.Verdicts
	}
	return parseAdherenceReply(resp.Text())
}

// parseAdherenceReply parses the strict-JSON adhere contract, tolerating a
// markdown fence around it. Malformed replies yield nothing (dropped, never
// stored — same philosophy as parseLessonReply). Fallback only — the primary
// contract is the forced adherenceTool call.
func parseAdherenceReply(text string) []adherenceVerdict {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "{"); i > 0 {
		text = text[i:]
	}
	if i := strings.LastIndex(text, "}"); i >= 0 {
		text = text[:i+1]
	}
	var p struct {
		Verdicts []adherenceVerdict `json:"verdicts"`
	}
	if json.Unmarshal([]byte(text), &p) != nil {
		return nil
	}
	return p.Verdicts
}
