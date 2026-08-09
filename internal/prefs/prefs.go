// Package prefs is the preference-learning reducer. It scans preference_signal
// events from the event log (the source of truth), clusters them by axis + lexical
// similarity, weighs each cluster with recency decay, tracks contradictions, and
// materializes the resulting candidates into the preference_candidates table
// atomically. Promotion to a standing plaintext rule (and demotion on
// contradiction) is a separate, silent step in StartChat (see promote.go) — this
// package is pure over the event log and makes no LLM calls.
package prefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/store"
)

// Thresholds and constants. The promotion gate (applyPreferencePromotions) uses
// promotionThreshold / minSignals / minSessions; the demotion gate uses
// contradictionThreshold. These are deliberately conservative: a one-off directive
// must NOT become a binding rule, and a single contradiction must NOT demote a
// standing preference.
const (
	maxScanSignals           = 5000 // hard cap on the event query (startup cost bound)
	promotionThreshold       = 2.0  // weighted score a candidate must reach to promote
	minSignals               = 3    // …and at least this many signals
	minSessions              = 2    // …across at least this many distinct sessions
	windowDays               = 30   // recency decay half-life: 0.5^((now-ts)/(30d))
	contradictionThreshold   = 2    // contradiction signal count that demotes a confirmed pref
	contradictionMinStrength = 0.7  // …only contradictions at/above this strength count
	jaccardThreshold         = 0.4  // lexical overlap to merge two same-axis signals

	// Adherence feedback (see foldAdherence): capped rehearsal boost for rules
	// the agent keeps following on accepted sessions.
	reinforceBoost = 0.3
	reinforceCap   = 1.0
)

// SignalRef is a pointer back to the event that contributed to a candidate — so
// the promotion notice and the introspect tool can show evidence (which sessions,
// what text). It's the "why did this become a rule" trail.
type SignalRef struct {
	SessionID string    `json:"session_id"`
	Text      string    `json:"text"`
	Strength  float64   `json:"strength"`
	TS        time.Time `json:"ts"`
	HeadSHA   string    `json:"head_sha,omitempty"` // repo HEAD when the directive was given — provenance
}

// Candidate is a clustered, weighted preference candidate — the reducer's output
// before promotion/demotion decides its fate. It maps 1:1 onto a
// store.PreferenceCandidate row.
type Candidate struct {
	ID            string
	Axis          string
	Text          string
	Scope         string
	Weight        float64
	SignalCount   int
	SessionCount  int
	FirstSeen     time.Time
	LastSeen      time.Time
	Status        string
	ConfirmedPath string // set when Status=="confirmed" — the exact plaintext file to revoke
	Evidence      []SignalRef

	// Contradictions is the count of same-axis signals with opposite polarity
	// that contradicted this candidate. Used by PendingDemotions to decide whether
	// a confirmed pref has been overturned.
	Contradictions int
}

// signalEvent is the parsed payload of a preference_signal event.
type signalEvent struct {
	Text      string
	Axis      string
	Scope     string
	Strength  float64
	SessionID string
	HeadSHA   string
	TS        time.Time
	Polarity  int // +1 affirmative, -1 negated (derived from Text)
}

// Reduce scans preference_signal events, clusters them, weighs them with recency
// decay, tracks contradictions, and materializes the candidates into the
// preference_candidates table atomically (ClearPreferenceCandidates + a batch of
// AddPreferenceCandidate in a single RunInTx — same pattern as learn.Run). It
// returns the candidates so the caller (applyPreferencePromotions) can decide
// promotions and demotions.
func Reduce(ctx context.Context, st store.Store) ([]Candidate, error) {
	evs, err := st.ListEvents(ctx, store.EventFilter{
		Kinds: []string{string(events.KindPreferenceSignal)},
		Limit: maxScanSignals,
	})
	if err != nil {
		return nil, fmt.Errorf("prefs reduce: list events: %w", err)
	}

	signals := make([]signalEvent, 0, len(evs))
	for _, e := range evs {
		sig, ok := parseSignal(e)
		if !ok {
			continue
		}
		signals = append(signals, sig)
	}

	clusters := clusterSignals(signals)
	now := time.Now().UTC()
	cands := make([]Candidate, 0, len(clusters))
	for _, cl := range clusters {
		cands = append(cands, buildCandidate(cl, signals, now))
	}

	// Fold in adherence verdicts (the self-correction loop): a violated verdict
	// on a session the human then corrected/rejected counts as a contradiction —
	// git-outcome pushback instead of verbal pushback; a followed verdict on an
	// accepted session is rehearsal (small, capped, decayed confirming weight).
	foldAdherence(ctx, st, cands, now)

	// Sort by weight descending so the materialized table and the promotion scan
	// present the strongest candidates first.
	sort.Slice(cands, func(i, j int) bool { return cands[i].Weight > cands[j].Weight })

	// Snapshot the prior lifecycle (status + confirmed path) by stable id BEFORE the
	// rebuild wipes it, so a confirmed/demoted candidate keeps its state across the
	// Reduce instead of resetting to "candidate" and re-promoting.
	prior := map[string]store.PreferenceCandidate{}
	if existing, err := st.ListPreferenceCandidates(ctx); err == nil {
		for _, e := range existing {
			prior[e.ID] = e
		}
	}

	// Materialize atomically: ClearPreferenceCandidates + AddPreferenceCandidate
	// batch in a single tx. A mid-batch failure rolls back to the old candidate
	// set instead of leaving a partial projection (same invariant as learn.Run's
	// claim rebuild).
	err = st.RunInTx(ctx, func(tx store.Tx) error {
		if err := tx.ClearPreferenceCandidates(ctx); err != nil {
			return err
		}
		for i := range cands {
			c := &cands[i]
			// Preserve the candidate's LIFECYCLE (status + confirmed path) across a
			// rebuild: a previously confirmed/demoted candidate (same stable id) keeps
			// that state instead of resetting to "candidate" and re-promoting. This is
			// what makes a confirmed pref survive a restart (paired with stableID). The
			// carried state is written back onto the returned candidate too, so the
			// promotion/demotion gates (which read c.Status) see the real lifecycle.
			if p, ok := prior[c.ID]; ok && p.Status != "" {
				c.Status, c.ConfirmedPath = p.Status, p.ConfirmedPath
			}
			status := c.Status
			if status == "" {
				status = "candidate"
			}
			pc := store.PreferenceCandidate{
				ID:            c.ID,
				Axis:          c.Axis,
				Text:          c.Text,
				Scope:         c.Scope,
				Weight:        c.Weight,
				SignalCount:   c.SignalCount,
				SessionCount:  c.SessionCount,
				FirstSeen:     c.FirstSeen,
				LastSeen:      c.LastSeen,
				Status:        status,
				ConfirmedPath: c.ConfirmedPath,
				Evidence:      evidenceJSON(c.Evidence),
			}
			if err := tx.AddPreferenceCandidate(ctx, pc); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("prefs reduce: materialize: %w", err)
	}
	return cands, nil
}

// foldAdherence merges KindAdherence verdicts into the candidates by rule id.
// Same shape as the lessons reducer's fold: violations tally toward the
// existing contradiction threshold, followed-and-accepted adds a capped,
// decayed reinforcement so a rule the agent keeps honoring doesn't decay away.
func foldAdherence(ctx context.Context, st store.Store, cands []Candidate, now time.Time) {
	evs, err := st.ListEvents(ctx, store.EventFilter{
		Kinds: []string{string(events.KindAdherence)}, Limit: maxScanSignals,
	})
	if err != nil {
		return
	}
	byID := map[string]*Candidate{}
	for i := range cands {
		byID[cands[i].ID] = &cands[i]
	}
	reinforce := map[string]float64{}
	for _, e := range evs {
		var p struct {
			RuleKind string `json:"rule_kind"`
			RuleID   string `json:"rule_id"`
			Verdict  string `json:"verdict"`
			Outcome  string `json:"outcome"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.RuleKind != "pref" {
			continue
		}
		c := byID[p.RuleID]
		if c == nil {
			continue
		}
		switch {
		case p.Verdict == "violated" && (p.Outcome == "corrected" || p.Outcome == "rejected"):
			c.Contradictions++
		case p.Verdict == "followed" && p.Outcome == "accepted":
			reinforce[p.RuleID] += reinforceBoost * decayFactor(now, e.TS)
		}
	}
	for id, boost := range reinforce {
		if boost > reinforceCap {
			boost = reinforceCap
		}
		byID[id].Weight += boost
	}
}

// parseSignal decodes a preference_signal event's payload into a signalEvent.
// Returns ok=false for malformed payloads (skipped, not fatal).
func parseSignal(e store.Event) (signalEvent, bool) {
	if len(e.Payload) == 0 {
		return signalEvent{}, false
	}
	var p struct {
		Text      string  `json:"text"`
		Axis      string  `json:"axis"`
		Scope     string  `json:"scope"`
		Strength  float64 `json:"strength"`
		SessionID string  `json:"session_id"`
		HeadSHA   string  `json:"head_sha"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return signalEvent{}, false
	}
	if p.Text == "" {
		return signalEvent{}, false
	}
	if p.Scope == "" {
		p.Scope = "."
	}
	return signalEvent{
		Text:      p.Text,
		Axis:      p.Axis,
		Scope:     p.Scope,
		Strength:  p.Strength,
		SessionID: p.SessionID,
		HeadSHA:   p.HeadSHA,
		TS:        e.TS,
		Polarity:  polarity(p.Text),
	}, true
}

// decayFactor is the recency decay: 0.5^((now-ts)/(windowDays*24h)). A signal
// loses half its weight every windowDays. A 30-day-old signal contributes 0.5×
// its strength; a 60-day-old one 0.25×. This keeps the weighted score responsive
// to the user's CURRENT preferences while letting stale one-offs rot away.
func decayFactor(now, ts time.Time) float64 {
	d := now.Sub(ts)
	if d <= 0 {
		return 1.0
	}
	halfLives := d.Hours() / (windowDays * 24)
	// 0.5^halfLives — integer part via loop, fractional part via linear interpolation
	// between the two surrounding half-lives (avoids importing math for one pow call).
	p := 1.0
	for i := 0; i < int(halfLives); i++ {
		p *= 0.5
	}
	frac := halfLives - float64(int(halfLives))
	p *= 1 - frac + frac*0.5
	return p
}

// buildCandidate folds a cluster of signals into a weighted Candidate. allSignals
// is the full signal set on all axes — it's passed so contradictions (same-axis,
// opposite-polarity signals in OTHER clusters) can be counted. A contradiction is
// a signal on the same axis with opposite polarity to this cluster, weighted at
// or above contradictionMinStrength — it represents the user pushing back against
// this preference.
func buildCandidate(cl cluster, allSignals []signalEvent, now time.Time) Candidate {
	var (
		weight      float64
		first, last time.Time
		sessions    = map[string]bool{}
		evidence    []SignalRef
		contradicts int
		repText     string
		maxStrength float64
	)
	for _, sig := range cl.signals {
		decay := decayFactor(now, sig.TS)
		weight += sig.Strength * decay
		if first.IsZero() || sig.TS.Before(first) {
			first = sig.TS
		}
		if sig.TS.After(last) {
			last = sig.TS
		}
		sessions[sig.SessionID] = true
		evidence = append(evidence, SignalRef{
			SessionID: sig.SessionID, Text: sig.Text, Strength: sig.Strength, TS: sig.TS, HeadSHA: sig.HeadSHA,
		})
		if sig.Strength > maxStrength {
			maxStrength = sig.Strength
			repText = sig.Text
		}
	}
	// Count contradictions: same-axis signals in OTHER clusters with opposite
	// polarity, at or above the contradiction strength floor.
	for _, sig := range allSignals {
		if sig.Axis != cl.axis {
			continue
		}
		if sig.Polarity == cl.polarity {
			continue
		}
		if sig.Strength >= contradictionMinStrength {
			contradicts++
		}
	}
	return Candidate{
		ID:             stableID(cl),
		Axis:           cl.axis,
		Text:           repText,
		Scope:          cl.scope,
		Weight:         weight,
		SignalCount:    len(cl.signals),
		SessionCount:   len(sessions),
		FirstSeen:      first,
		LastSeen:       last,
		Status:         "candidate",
		Evidence:       evidence,
		Contradictions: contradicts,
	}
}

// stableID derives a DETERMINISTIC candidate id from the cluster's identity —
// axis, polarity, scope, and the sorted union of its signal tokens. This is the
// linchpin of the whole feature: a random id per Reduce meant the same preference
// re-promoted every session (a new duplicate file each launch) and "confirmed"
// status never survived a restart. A content-derived id makes a candidate the SAME
// row across runs, so promotion is idempotent and confirmed/demoted state persists.
func stableID(cl cluster) string {
	toks := map[string]bool{}
	for _, sig := range cl.signals {
		for t := range tokens(sig.Text) {
			toks[t] = true
		}
	}
	keys := make([]string, 0, len(toks))
	for t := range toks {
		keys = append(keys, t)
	}
	sort.Strings(keys)
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s", cl.axis, cl.polarity, cl.scope, strings.Join(keys, ","))))
	return "prefcand_" + hex.EncodeToString(h[:6])
}

// evidenceJSON marshals the evidence refs for the evidence_json column.
func evidenceJSON(ev []SignalRef) json.RawMessage {
	if len(ev) == 0 {
		return nil
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	return b
}
