package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/prefs"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/store"
)

// preferenceSignalTool captures a durable user preference/constraint. It appends a
// preference_signal event to the event log — nothing more. The reducer (prefs.Reduce)
// clusters these and promotes a cluster to a standing plaintext preference once it
// crosses the evidence bar. This tool NEVER writes to .memcode/prefs/; only the
// silent promotion in StartChat does (TestPreferenceSignalToolDoesNotWritePrefs).
func (s *Session) preferenceSignalTool(ctx context.Context, input json.RawMessage) toolResult {
	var in tools.PreferenceSignalInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult(err.Error())
	}
	if strings.TrimSpace(in.Text) == "" {
		return errResult("preference_signal needs `text`.")
	}
	// Normalize the axis to the known vocabulary — an arbitrary/empty axis from the
	// model creates junk partitions that can still promote. Unknown → "other".
	axis := normalizeAxis(in.Axis)
	// Strength is amplified by the room: a directive issued under repair mode
	// (MemoryWeight="strong") or high friction carries more weight than a casual
	// mention. The reducer folds strength into the weighted score with decay.
	strength := 0.5
	if s.room.Policy.MemoryWeight == "strong" {
		strength = 0.9
	}
	if s.room.Friction == "high" {
		strength = math.Max(strength, 0.8)
	}
	// Provenance guard: a signal is meant to capture the USER'S directive. When the room
	// hasn't already vouched for it (no repair-mode / high-friction amplification) AND the
	// text shares no meaningful overlap with what the user actually said this turn, it is
	// likely model-originated (paraphrase, or induced by tool output / a web page) — floor
	// its strength so it can't accumulate into a binding rule without the user genuinely
	// repeating it. This is the drift/injection backstop that stands in for the HITL
	// confirm (deliberately waived). Room amplification IS provenance, so it's exempt.
	roomVouched := s.room.Policy.MemoryWeight == "strong" || s.room.Friction == "high"
	if !roomVouched && !overlapsUserText(in.Text, s.lastUserText) {
		strength = math.Min(strength, 0.2)
	}
	scope := in.Scope
	if scope == "" {
		scope = "."
	}
	s.emit(ctx, events.KindPreferenceSignal, map[string]any{
		"text":       in.Text,
		"axis":       axis,
		"scope":      scope,
		"strength":   strength,
		"room_mode":  string(s.room.Mode),
		"session_id": s.sessionID,
		"head_sha":   s.headSHA,
	})
	// Also write the CANONICAL copy to the append-only session log (events.jsonl). The
	// SQLite events table the reducer queries is a derived index; the file is the source
	// of truth, so a signal survives a state.db rebuild (backfilled by backfillPrefSignals).
	s.slog.Append(sessionlog.Record{
		Kind: sessionlog.KindPreferenceSignal, Text: in.Text, Axis: axis, Scope: scope, Strength: strength,
		HeadSHA: s.headSHA,
	})
	s.toolLine(true, "PrefSignal", axis, clip(in.Text, 60), false)
	return textResult("Signal captured. It will accumulate with similar directives and promote to a standing preference if it recurs across sessions.")
}

// normalizeAxis maps a model-supplied axis onto the known vocabulary
// (tools.PreferenceAxes); anything else becomes "other" so junk axes can't create
// unbounded partitions.
func normalizeAxis(axis string) string {
	a := strings.ToLower(strings.TrimSpace(axis))
	for _, known := range tools.PreferenceAxes {
		if a == known {
			return a
		}
	}
	return "other"
}

// overlapsUserText reports whether directive shares a meaningful word with the user's
// latest text — a cheap provenance check. Empty user text (e.g. a background turn)
// is treated as NO overlap, so autonomous signals stay at floor strength.
func overlapsUserText(directive, userText string) bool {
	if strings.TrimSpace(userText) == "" {
		return false
	}
	ut := strings.ToLower(userText)
	for _, w := range strings.FieldsFunc(strings.ToLower(directive), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(w) >= 4 && strings.Contains(ut, w) {
			return true
		}
	}
	return false
}

// applyPreferencePromotions is the silent, deterministic promotion/demotion step
// called from StartChat. It runs the reducer over the preference_signal event log,
// promotes any candidate that has crossed the evidence bar (≥3 signals, ≥2
// sessions, weighted score ≥ 2.0) to a standing plaintext rule in .memcode/prefs/,
// and demotes any confirmed preference that has been contradicted enough (≥2
// contradiction signals). No s.ask, no confirm card — the threshold IS the gate.
// Safe in StartChat (no turn-boundary interaction) in all three front-ends.
func (s *Session) applyPreferencePromotions(ctx context.Context) {
	s.backfillPrefSignals(ctx) // recover canonical signals into SQLite if the derived index was wiped
	cands, err := prefs.Reduce(ctx, s.store)
	if err != nil {
		s.printf("  preference reduce error: %v\n", err)
		return
	}

	// Promote: write the plaintext file, persist status AND confirmed_path, emit a
	// decision event, and print a startup notice.
	for _, top := range prefs.PendingPromotions(cands) {
		path, err := s.writeConfirmedPref(ctx, top)
		if err != nil {
			s.printf("  preference promote error: %v\n", err)
			continue
		}
		// Persist status AND confirmed_path so the candidate table knows where the
		// binding file lives. UpdatePreferenceCandidateStatus takes the path
		// explicitly — the demotion path passes "".
		if err := s.store.UpdatePreferenceCandidateStatus(ctx, top.ID, "confirmed", path, top.Weight); err != nil {
			s.printf("  preference promote error (persist): %v\n", err)
			continue
		}
		s.emit(ctx, events.KindDecision, map[string]any{
			"kind": "preference_promoted", "axis": top.Axis, "text": top.Text, "path": path,
		})
		s.printf("  ℹ promoted standing preference: [%s] %s\n", top.Axis, top.Text)
	}

	// Demote: delete the plaintext file, clear confirmed_path, emit a decision
	// event, and print a startup notice.
	demotions, err := prefs.PendingDemotions(cands, s.store)
	if err != nil {
		s.printf("  preference demote error: %v\n", err)
		return
	}
	for _, top := range demotions {
		// Remove the EXACT stored file (<id>-<slug>.md). Fall back to the glob resolver
		// if the stored path is missing — never the bare <id>.md, which never existed.
		path := top.ConfirmedPath
		if path == "" {
			path = prefs.ConfirmPath(s.root, top.ID)
		}
		_ = os.Remove(path)
		if err := s.store.UpdatePreferenceCandidateStatus(ctx, top.ID, "demoted", "", 0); err != nil {
			s.printf("  preference demote error (persist): %v\n", err)
			continue
		}
		s.emit(ctx, events.KindDecision, map[string]any{
			"kind": "preference_demoted", "axis": top.Axis, "text": top.Text,
		})
		s.printf("  ℹ demoted standing preference (contradicted): [%s] %s\n", top.Axis, top.Text)
	}
}

// backfillPrefSignals recovers preference signals from the CANONICAL append-only logs
// (events.jsonl) into the SQLite events table when that derived index has none — e.g.
// after a state.db wipe/rebuild. Steady-state this is a no-op (the table already has
// them, dual-written at capture time), so it can't double-count.
func (s *Session) backfillPrefSignals(ctx context.Context) {
	existing, err := s.store.ListEvents(ctx, store.EventFilter{
		Kinds: []string{string(events.KindPreferenceSignal)}, Limit: 1,
	})
	if err != nil || len(existing) > 0 {
		return // table already populated (or unreadable) — nothing to recover
	}
	recs, err := sessionlog.PreferenceSignals(s.root)
	if err != nil || len(recs) == 0 {
		return
	}
	failed := 0
	for _, r := range recs {
		if _, err := s.store.AppendEvent(ctx, store.Event{
			TS:   r.TS,
			Kind: string(events.KindPreferenceSignal),
			Payload: mustJSON(map[string]any{
				"text": r.Text, "axis": r.Axis, "scope": r.Scope,
				"strength": r.Strength, "session_id": r.SessionID, "head_sha": r.HeadSHA,
			}),
		}); err != nil {
			failed++
		}
	}
	recovered := len(recs) - failed
	s.printf("  ℹ recovered %d preference signal(s) from the session log\n", recovered)
	if failed > 0 {
		s.printf("⚠ %d/%d preference signals couldn't be recovered\n", failed, len(recs))
	}
}

// mustJSON marshals a payload for an event; returns nil on failure (a dropped backfill
// row is recoverable on the next launch — never worth panicking a session start).
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// writeConfirmedPref writes the standing preference as a plaintext markdown file
// under .memcode/prefs/<id>-<slug>.md. The slug is the first 4 words of the pref
// text, lowercased and hyphenated. The file carries the pref text, axis, scope,
// confirmation date, weight, and evidence links (which sessions contributed).
// Returns the absolute path so the caller can persist it via
// UpdatePreferenceCandidateStatus.
func (s *Session) writeConfirmedPref(ctx context.Context, c prefs.Candidate) (string, error) {
	dir := filepath.Join(s.root, ".memcode", "prefs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create prefs dir: %w", err)
	}
	slug := slugify(c.Text, 4)
	filename := c.ID + "-" + slug + ".md"
	path := filepath.Join(dir, filename)

	var b strings.Builder
	fmt.Fprintf(&b, "# Standing preference: %s\n\n", c.Text)
	fmt.Fprintf(&b, "- **axis:** %s\n", c.Axis)
	fmt.Fprintf(&b, "- **scope:** %s\n", c.Scope)
	fmt.Fprintf(&b, "- **weight:** %.2f\n", c.Weight)
	fmt.Fprintf(&b, "- **confirmed:** %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- **signals:** %d across %d session(s)\n", c.SignalCount, c.SessionCount)
	if len(c.Evidence) > 0 {
		fmt.Fprintf(&b, "\n## Evidence (contributing signals)\n")
		for _, ev := range c.Evidence {
			fmt.Fprintf(&b, "- [%s] %s (strength %.2f, %s)\n",
				ev.SessionID, ev.Text, ev.Strength, ev.TS.Format("2006-01-02"))
		}
	}
	fmt.Fprintf(&b, "\n<!-- This is a standing preference learned from your repeated directives. -->\n")
	fmt.Fprintf(&b, "<!-- Edit or delete this file to change or revert it. `memcode{command:\"preferences\"}` reviews all. -->\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("write pref file: %w", err)
	}
	return path, nil
}

// slugify derives a filesystem-safe slug from text: the first n words, lowercased,
// non-alphanumerics replaced with hyphens, trailing hyphens trimmed.
func slugify(text string, n int) string {
	words := 0
	var out strings.Builder
	for _, r := range strings.ToLower(text) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			out.WriteRune(r)
		} else if r == ' ' || r == '\t' || r == ',' || r == ';' {
			if out.Len() > 0 && !strings.HasSuffix(out.String(), "-") {
				words++
				if words >= n {
					break
				}
				out.WriteRune('-')
			}
		} else if out.Len() > 0 && !strings.HasSuffix(out.String(), "-") {
			words++
			if words >= n {
				break
			}
			out.WriteRune('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

// inlinePrefs reads confirmed preference files from .memcode/prefs/*.md, ranks
// them by the weight: field in the file header, and returns the top 5 as a
// concise ≤10-line block for injection into the system prompt. Returns "" when
// there are no confirmed prefs. This is the surfacing layer — the model sees
// standing preferences as standing context, ranked by evidence weight.
func (s *Session) inlinePrefs(ctx context.Context) string {
	block, _ := s.inlinePrefsTop(ctx)
	return block
}

// inlinePrefsTop is inlinePrefs plus the stable ids of the prefs actually
// surfaced — recorded as context_inlined so the adherence judge only scores
// rules the model really saw.
func (s *Session) inlinePrefsTop(ctx context.Context) (string, []string) {
	dir := filepath.Join(s.root, ".memcode", "prefs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil // no prefs dir yet — normal
	}
	var prefsList []prefFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		p := parsePrefFile(string(data))
		p.id = prefIDFromFilename(e.Name())
		if p.text == "" {
			continue
		}
		prefsList = append(prefsList, p)
	}
	if len(prefsList) == 0 {
		return "", nil
	}
	// Rank by weight descending; take top 5 (the hard cap).
	sortPrefs(prefsList)
	if len(prefsList) > 5 {
		prefsList = prefsList[:5]
	}
	var b strings.Builder
	var ids []string
	b.WriteString("STANDING PREFERENCES (learned from your directives, ranked by weight):\n")
	for _, p := range prefsList {
		fmt.Fprintf(&b, "- [%s] %s\n", p.axis, p.text)
		ids = append(ids, p.id)
	}
	out := strings.TrimRight(b.String(), "\n")
	// Hard cap: 10 lines. The header + 5 prefs = 6 lines; this is a safety valve
	// for a future where the format grows.
	if lines := strings.Count(out, "\n") + 1; lines > 10 {
		// Trim to 10 lines.
		parts := strings.SplitN(out, "\n", 11)
		out = strings.Join(parts[:10], "\n")
	}
	return out, ids
}

// prefIDFromFilename recovers the stable candidate id from "<id>-<slug>.md"
// (ids are "pref_<hex>"-style with no dash, so the id is everything before the
// first dash).
func prefIDFromFilename(name string) string {
	name = strings.TrimSuffix(name, ".md")
	if i := strings.Index(name, "-"); i > 0 {
		return name[:i]
	}
	return name
}

// prefFile is the parsed view of a .memcode/prefs/*.md file — the fields
// inlinePrefs needs to rank and render.
type prefFile struct {
	id     string // stable candidate id, from the filename prefix
	axis   string
	text   string
	weight float64
}

// parsePrefFile extracts the axis, text, and weight from a confirmed-preference
// markdown file. The text is the first H1; axis and weight come from the
// `- **field:**` lines.
func parsePrefFile(content string) prefFile {
	var p prefFile
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			p.text = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			continue
		}
		if strings.HasPrefix(line, "- **axis:**") {
			p.axis = strings.TrimSpace(strings.TrimPrefix(line, "- **axis:**"))
			continue
		}
		if strings.HasPrefix(line, "- **weight:**") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "- **weight:**"))
			fmt.Sscanf(val, "%f", &p.weight)
		}
	}
	return p
}

// sortPrefs sorts by weight descending (simple sort for the small slice).
func sortPrefs(p []prefFile) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].weight > p[j-1].weight; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}
