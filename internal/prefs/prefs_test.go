package prefs

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/store"
)

func seedStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seed appends a preference_signal event with the given fields. strength defaults
// to 0.5 if zero. ts defaults to now if zero.
func seed(t *testing.T, s store.Store, text, axis, session string, strength float64, ts time.Time) {
	t.Helper()
	if strength == 0 {
		strength = 0.5
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	payload := map[string]any{
		"text":       text,
		"axis":       axis,
		"scope":      ".",
		"strength":   strength,
		"session_id": session,
	}
	// Append the event directly so we can set the timestamp (events.Append uses now).
	raw := mustJSON(payload)
	if _, err := s.AppendEvent(context.Background(), store.Event{
		TS:      ts,
		Kind:    string(events.KindPreferenceSignal),
		Actor:   "agent",
		Payload: raw,
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestReduceWeight(t *testing.T) {
	ctx := context.Background()
	s := seedStore(t)
	now := time.Now().UTC()

	// 3 signals, same session: strength 0.9, 0.5, 0.5, all fresh (decay≈1).
	seed(t, s, "always commit before deploy", "workflow", "s1", 0.9, now)
	seed(t, s, "always commit before deploy", "workflow", "s1", 0.5, now)
	seed(t, s, "always commit before deploy", "workflow", "s1", 0.5, now)

	cands, err := Reduce(ctx, s)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d: %+v", len(cands), cands)
	}
	// weight ≈ 0.9 + 0.5 + 0.5 = 1.9 (fresh, decay≈1). Below promotion threshold
	// (2.0) because only 1 session.
	if cands[0].Weight < 1.8 || cands[0].Weight > 2.0 {
		t.Errorf("weight = %v, want ~1.9", cands[0].Weight)
	}
	if cands[0].SignalCount != 3 {
		t.Errorf("signal_count = %d, want 3", cands[0].SignalCount)
	}
	if cands[0].SessionCount != 1 {
		t.Errorf("session_count = %d, want 1", cands[0].SessionCount)
	}
}

func TestReduceOneOffDecays(t *testing.T) {
	ctx := context.Background()
	s := seedStore(t)
	// A single 60-day-old signal at strength 0.5 → decay 0.25 → weight 0.125.
	// Far below any threshold; a single one-off must not become a standing rule.
	seed(t, s, "stop using semicolons", "style", "s1", 0.5, time.Now().UTC().Add(-60*24*time.Hour))

	cands, err := Reduce(ctx, s)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(cands))
	}
	if cands[0].Weight > 0.5 {
		t.Errorf("decayed weight = %v, want ≤ 0.5 (one-off should rot)", cands[0].Weight)
	}
	if cands[0].SignalCount != 1 || cands[0].SessionCount != 1 {
		t.Errorf("one-off should have 1 signal/1 session, got %d/%d", cands[0].SignalCount, cands[0].SessionCount)
	}
}

func TestReduceRecurringPromotes(t *testing.T) {
	ctx := context.Background()
	s := seedStore(t)
	now := time.Now().UTC()

	// 3 signals across 2 sessions, fresh, strength 0.9 each → weight ≈ 2.7.
	// This crosses the promotion bar (≥2.0, ≥3 signals, ≥2 sessions).
	seed(t, s, "always commit, deploy, rebuild after gateway changes", "workflow", "s1", 0.9, now)
	seed(t, s, "always commit, deploy, rebuild after gateway changes", "workflow", "s2", 0.9, now.Add(-2*24*time.Hour))
	seed(t, s, "commit deploy rebuild after changes", "workflow", "s2", 0.9, now.Add(-4*24*time.Hour))

	cands, err := Reduce(ctx, s)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d: %+v", len(cands), cands)
	}
	if cands[0].Weight < promotionThreshold {
		t.Errorf("weight = %v, want ≥ %v (should cross promotion bar)", cands[0].Weight, promotionThreshold)
	}
	if cands[0].SessionCount < minSessions {
		t.Errorf("session_count = %d, want ≥ %d", cands[0].SessionCount, minSessions)
	}
	if cands[0].SignalCount < minSignals {
		t.Errorf("signal_count = %d, want ≥ %d", cands[0].SignalCount, minSignals)
	}
}

// The candidate id must be STABLE across Reduce runs (content-derived, not random),
// or the same preference re-promotes every session and confirmed status never sticks.
func TestReduceStableIdentity(t *testing.T) {
	ctx := context.Background()
	s := seedStore(t)
	now := time.Now().UTC()
	seed(t, s, "always run gofmt before committing", "workflow", "s1", 0.9, now)
	seed(t, s, "always run gofmt before committing", "workflow", "s2", 0.9, now.Add(-24*time.Hour))

	first, err := Reduce(ctx, s)
	if err != nil || len(first) == 0 {
		t.Fatalf("Reduce 1: %v (%d cands)", err, len(first))
	}
	id1 := first[0].ID

	// A second Reduce over the SAME signals must yield the SAME id.
	second, err := Reduce(ctx, s)
	if err != nil || len(second) == 0 {
		t.Fatalf("Reduce 2: %v", err)
	}
	if second[0].ID != id1 {
		t.Fatalf("candidate id changed across Reduce: %q → %q (must be deterministic)", id1, second[0].ID)
	}

	// Confirm the candidate, then Reduce again — the confirmed status must PERSIST
	// (not reset to "candidate" and re-promote).
	if err := s.UpdatePreferenceCandidateStatus(ctx, id1, "confirmed", "/p/"+id1+"-x.md", second[0].Weight); err != nil {
		t.Fatal(err)
	}
	third, err := Reduce(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if third[0].Status != "confirmed" {
		t.Fatalf("status after re-Reduce = %q, want confirmed (must survive restart)", third[0].Status)
	}
	if len(PendingPromotions(third)) != 0 {
		t.Fatal("a confirmed candidate must NOT appear in PendingPromotions again")
	}
	if third[0].ConfirmedPath == "" {
		t.Fatal("ConfirmedPath must be preserved across Reduce")
	}
}

// The gating axis is captured but never auto-promoted (permission-adjacent).
func TestGatingAxisNotPromoted(t *testing.T) {
	ctx := context.Background()
	s := seedStore(t)
	now := time.Now().UTC()
	seed(t, s, "always allow rm in this repo", "gating", "s1", 0.9, now)
	seed(t, s, "always allow rm in this repo", "gating", "s2", 0.9, now.Add(-24*time.Hour))
	seed(t, s, "always allow rm here", "gating", "s3", 0.9, now.Add(-48*time.Hour))
	cands, err := Reduce(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(PendingPromotions(cands)) != 0 {
		t.Fatal("gating-axis candidate must not be eligible for auto-promotion")
	}
}

func TestReduceBoundedScan(t *testing.T) {
	ctx := context.Background()
	s := seedStore(t)
	// Seed 6000 events — Reduce must load only maxScanSignals (5000).
	for i := 0; i < 6000; i++ {
		seed(t, s, "bounded scan test", "style", "s1", 0.5, time.Now().UTC())
	}
	cands, err := Reduce(ctx, s)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	// All 6000 signals are identical → they'd cluster into ONE candidate, but
	// only 5000 were loaded. The point is the scan is bounded, not the cluster
	// count. Assert the candidate exists and has ≤5000 signals.
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate (all identical), got %d", len(cands))
	}
	if cands[0].SignalCount > maxScanSignals {
		t.Errorf("signal_count = %d, want ≤ %d (scan must be bounded)", cands[0].SignalCount, maxScanSignals)
	}
}

func TestClusteringMergesParaphrases(t *testing.T) {
	ctx := context.Background()
	s := seedStore(t)
	now := time.Now().UTC()
	// Two phrasings with high token overlap ("commit deploy rebuild" shared).
	seed(t, s, "commit, deploy, rebuild after gateway changes", "workflow", "s1", 0.9, now)
	seed(t, s, "commit, push, ship it after gateway changes", "workflow", "s2", 0.9, now.Add(-time.Hour))

	cands, err := Reduce(ctx, s)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	// They share "commit", "after", "gateway", "changes" — Jaccard ≥ 0.4.
	if len(cands) != 1 {
		t.Fatalf("expected 1 merged candidate, got %d: %+v", len(cands), cands)
	}
}

func TestClusteringSeparatesAxes(t *testing.T) {
	ctx := context.Background()
	s := seedStore(t)
	now := time.Now().UTC()
	seed(t, s, "always commit before deploy", "workflow", "s1", 0.9, now)
	seed(t, s, "use pnpm never npm", "style", "s2", 0.9, now)

	cands, err := Reduce(ctx, s)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates (different axes), got %d: %+v", len(cands), cands)
	}
	axes := map[string]bool{cands[0].Axis: true, cands[1].Axis: true}
	if !axes["workflow"] || !axes["style"] {
		t.Errorf("expected workflow + style axes, got %s / %s", cands[0].Axis, cands[1].Axis)
	}
}

func TestContradictionDetection(t *testing.T) {
	ctx := context.Background()
	s := seedStore(t)
	now := time.Now().UTC()
	// "always commit" (affirmative) + "never commit" (negated) on the same axis.
	seed(t, s, "always commit before deploy", "workflow", "s1", 0.9, now)
	seed(t, s, "never commit before deploy", "workflow", "s2", 0.9, now)

	cands, err := Reduce(ctx, s)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	// Two separate clusters (opposite polarity), each recording the other as a
	// contradiction. Both should have Contradictions ≥ 1.
	if len(cands) != 2 {
		t.Fatalf("expected 2 clusters (opposite polarity), got %d: %+v", len(cands), cands)
	}
	for _, c := range cands {
		if c.Contradictions < 1 {
			t.Errorf("candidate [%s] %q: contradictions = %d, want ≥ 1", c.Axis, c.Text, c.Contradictions)
		}
	}
}

// TestReduceMaterializesAtomically: Reduce writes candidates in a single tx; a
// failure mid-batch must roll back so the old candidate set survives.
func TestReduceMaterializesAtomically(t *testing.T) {
	ctx := context.Background()
	s := seedStore(t)

	// Seed an initial candidate directly so we have something to preserve.
	if err := s.AddPreferenceCandidate(ctx, store.PreferenceCandidate{
		ID: "prefcand_survivor", Axis: "style", Text: "survives rollback",
		Weight: 2.0, SignalCount: 3, SessionCount: 2, Status: "candidate",
		FirstSeen: time.Now().UTC(), LastSeen: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	// Seed one signal so Reduce has something to materialize.
	seed(t, s, "atomicity test signal", "workflow", "s1", 0.5, time.Now().UTC())

	// Inject a failing Store wrapper that fails after the Nth AddPreferenceCandidate.
	wrap := &failingStore{Store: s, failAfter: 0}
	if _, err := Reduce(ctx, wrap); err == nil {
		t.Fatal("expected Reduce to fail with injected error, got nil")
	}

	// The rollback must preserve the seeded candidate.
	got, err := s.ListPreferenceCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPreferenceCandidates: %v", err)
	}
	found := false
	for _, c := range got {
		if c.ID == "prefcand_survivor" {
			found = true
		}
	}
	if !found {
		t.Error("rollback failed: seeded candidate 'prefcand_survivor' was lost")
	}
}

// failingStore wraps a Store and makes RunInTx fail after the inner tx AddPreferenceCandidate
// is called failAfter times. It delegates everything else to the real store.
type failingStore struct {
	store.Store
	failAfter int
}

func (f *failingStore) RunInTx(ctx context.Context, fn func(store.Tx) error) error {
	return f.Store.RunInTx(ctx, func(tx store.Tx) error {
		// Wrap the tx so we can count AddPreferenceCandidate calls and fail.
		wtx := &countingTx{Tx: tx, failAfter: f.failAfter}
		err := fn(wtx)
		if err != nil {
			return err
		}
		if wtx.calls > f.failAfter {
			return errors.New("injected mid-batch failure")
		}
		return nil
	})
}

type countingTx struct {
	store.Tx
	failAfter int
	calls     int
}

func (c *countingTx) AddPreferenceCandidate(ctx context.Context, pc store.PreferenceCandidate) error {
	c.calls++
	if c.calls > c.failAfter {
		return errors.New("injected AddPreferenceCandidate failure")
	}
	return c.Tx.AddPreferenceCandidate(ctx, pc)
}

// Adherence verdicts fold into the candidates: violated-on-corrected counts as
// a contradiction (git pushback), followed-on-accepted reinforces (capped).
func TestFoldAdherencePrefs(t *testing.T) {
	s := seedStore(t)
	for i, sess := range []string{"s1", "s2", "s3"} {
		seed(t, s, "always run the linter before committing changes", "workflow", sess, 0.9,
			time.Now().UTC().Add(-time.Duration(i+1)*time.Hour))
	}
	cands, err := Reduce(context.Background(), s)
	if err != nil || len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d (err %v)", len(cands), err)
	}
	id, base := cands[0].ID, cands[0].Weight
	baseContra := cands[0].Contradictions

	addAdh := func(verdict, outcome string) {
		payload := mustJSON(map[string]any{
			"rule_kind": "pref", "rule_id": id, "verdict": verdict, "outcome": outcome,
		})
		if _, err := s.AppendEvent(context.Background(), store.Event{
			TS: time.Now().UTC(), Kind: string(events.KindAdherence), Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	addAdh("violated", "corrected")
	addAdh("violated", "accepted") // must not count
	addAdh("followed", "accepted")

	cands, err = Reduce(context.Background(), s)
	if err != nil || len(cands) != 1 {
		t.Fatal(err)
	}
	if got := cands[0].Contradictions - baseContra; got != 1 {
		t.Errorf("adherence contradictions = %d, want 1", got)
	}
	if cands[0].Weight <= base || cands[0].Weight > base+reinforceCap+0.001 {
		t.Errorf("weight %f should gain a capped reinforcement over %f", cands[0].Weight, base)
	}
}

// head_sha provenance rides the signal into the candidate's evidence refs.
func TestSignalHeadSHAProvenance(t *testing.T) {
	s := seedStore(t)
	payload := mustJSON(map[string]any{
		"text": "prefer table-driven tests for parsers", "axis": "style", "scope": ".",
		"strength": 0.9, "session_id": "s1", "head_sha": "fee1dead0000",
	})
	if _, err := s.AppendEvent(context.Background(), store.Event{
		TS: time.Now().UTC(), Kind: string(events.KindPreferenceSignal), Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
	cands, err := Reduce(context.Background(), s)
	if err != nil || len(cands) != 1 || len(cands[0].Evidence) != 1 {
		t.Fatalf("want 1 candidate with 1 evidence ref (err %v): %+v", err, cands)
	}
	if cands[0].Evidence[0].HeadSHA != "fee1dead0000" {
		t.Errorf("evidence head_sha = %q, want fee1dead0000", cands[0].Evidence[0].HeadSHA)
	}
}
