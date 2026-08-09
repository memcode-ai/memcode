package lessons

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/store"
)

func openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func addSignal(t *testing.T, st store.Store, trigger, strategy, session string, strength float64, age time.Duration) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"trigger": trigger, "strategy": strategy, "strength": strength, "session_id": session,
	})
	if _, err := st.AppendEvent(context.Background(), store.Event{
		TS: time.Now().UTC().Add(-age), Kind: string(events.KindLessonSignal), Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
}

const (
	trigGoImports = "edited Go file breaks the parse after adding imports"
	stratGofmt    = "run diagnostics on the edited Go file before finishing instead of assuming the import block is balanced"
)

func TestReduceClustersSimilarSignals(t *testing.T) {
	st := openStore(t)
	addSignal(t, st, trigGoImports, stratGofmt, "s1", 0.6, time.Hour)
	addSignal(t, st, "edited Go file breaks parse when adding imports", stratGofmt, "s2", 0.6, 2*time.Hour)
	addSignal(t, st, "tui renders stale footer after resize", "wrap scrollback lines to the terminal width before appending", "s3", 0.6, time.Hour)

	cands, err := Reduce(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 clusters (imports + tui), got %d: %+v", len(cands), cands)
	}
	top := cands[0]
	if top.SignalCount != 2 || top.SessionCount != 2 {
		t.Errorf("import cluster should merge 2 signals across 2 sessions: %+v", top)
	}
}

func TestPromotionRigor(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	// Two signals in ONE session: under both the signal and session floors.
	addSignal(t, st, trigGoImports, stratGofmt, "s1", 0.9, time.Hour)
	addSignal(t, st, trigGoImports, stratGofmt, "s1", 0.9, time.Hour)
	cands, _ := Reduce(context.Background(), st)
	if got := PendingPromotions(root, cands); len(got) != 0 {
		t.Fatalf("2 signals / 1 session must NOT promote, got %+v", got)
	}
	// Third signal, second session: crosses every bar (3 × 0.9 fresh ≈ 2.7).
	addSignal(t, st, trigGoImports, stratGofmt, "s2", 0.9, time.Hour)
	cands, _ = Reduce(context.Background(), st)
	pending := PendingPromotions(root, cands)
	if len(pending) != 1 {
		t.Fatalf("3 signals / 2 sessions should promote exactly one, got %+v", pending)
	}
	path, err := WriteLesson(root, pending[0])
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	for _, want := range []string{"# Lesson:", "**strategy:**", "## Evidence", "[s1]", "[s2]"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("lesson file missing %q:\n%s", want, body)
		}
	}
	// Idempotent: once written, it's no longer pending.
	cands, _ = Reduce(context.Background(), st)
	if got := PendingPromotions(root, cands); len(got) != 0 {
		t.Errorf("written lesson must not re-promote, got %+v", got)
	}
}

func TestDecayFadesOldFailures(t *testing.T) {
	st := openStore(t)
	// Three ancient signals across two sessions: counts pass, weight must not.
	addSignal(t, st, trigGoImports, stratGofmt, "s1", 0.9, 200*24*time.Hour)
	addSignal(t, st, trigGoImports, stratGofmt, "s2", 0.9, 200*24*time.Hour)
	addSignal(t, st, trigGoImports, stratGofmt, "s2", 0.9, 200*24*time.Hour)
	cands, _ := Reduce(context.Background(), st)
	if got := PendingPromotions(t.TempDir(), cands); len(got) != 0 {
		t.Errorf("stale failures must decay below the bar, got %+v", got)
	}
}

func TestInlineSurfacesTopLessons(t *testing.T) {
	root := t.TempDir()
	if Inline(root) != "" {
		t.Fatal("no lessons dir → empty block")
	}
	c := Candidate{ID: "lesson_ab12cd34", Trigger: trigGoImports, Strategy: stratGofmt,
		Weight: 2.4, SignalCount: 3, SessionCount: 2,
		Evidence: []Signal{{Trigger: trigGoImports, Strategy: stratGofmt, Strength: 0.9, SessionID: "s1", TS: time.Now()}}}
	if _, err := WriteLesson(root, c); err != nil {
		t.Fatal(err)
	}
	block := Inline(root)
	if !strings.Contains(block, "LESSONS FROM PAST FAILURES") {
		t.Errorf("header missing: %q", block)
	}
	if !strings.Contains(block, "not instructions") {
		t.Errorf("data-framing missing (poisoning boundary): %q", block)
	}
	if !strings.Contains(block, trigGoImports) || !strings.Contains(block, stratGofmt) {
		t.Errorf("lesson content missing: %q", block)
	}
	// Deleting the file revokes the lesson.
	entries, _ := os.ReadDir(filepath.Join(root, ".memcode", "lessons"))
	for _, e := range entries {
		_ = os.Remove(filepath.Join(root, ".memcode", "lessons", e.Name()))
	}
	if Inline(root) != "" {
		t.Error("deleted lesson must vanish from the block")
	}
}

func TestStableIDIsContentDerived(t *testing.T) {
	a := []Signal{{Trigger: trigGoImports, Strategy: stratGofmt}}
	b := []Signal{{Trigger: trigGoImports, Strategy: stratGofmt}}
	if stableID(a) != stableID(b) {
		t.Error("same content must yield the same id")
	}
	c := []Signal{{Trigger: "totally different failure shape entirely", Strategy: "do the other thing instead"}}
	if stableID(a) == stableID(c) {
		t.Error("different content must yield different ids")
	}
}

func addAdherence(t *testing.T, st store.Store, ruleID, verdict, outcome string, age time.Duration) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"rule_kind": "lesson", "rule_id": ruleID, "verdict": verdict, "outcome": outcome,
	})
	if _, err := st.AppendEvent(context.Background(), store.Event{
		TS: time.Now().UTC().Add(-age), Kind: string(events.KindAdherence), Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}
}

// The self-correction loop: violated verdicts on corrected/rejected sessions
// tally toward demotion; followed verdicts on accepted sessions reinforce
// (capped) and never demote.
func TestFoldAdherence(t *testing.T) {
	st := openStore(t)
	addSignal(t, st, trigGoImports, stratGofmt, "s1", 0.9, time.Hour)
	addSignal(t, st, trigGoImports, stratGofmt, "s2", 0.9, 2*time.Hour)
	addSignal(t, st, trigGoImports, stratGofmt, "s3", 0.9, 3*time.Hour)
	cands, err := Reduce(context.Background(), st)
	if err != nil || len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d (err %v)", len(cands), err)
	}
	id := cands[0].ID
	base := cands[0].Weight

	addAdherence(t, st, id, "violated", "corrected", time.Hour)
	addAdherence(t, st, id, "violated", "rejected", time.Hour)
	addAdherence(t, st, id, "violated", "accepted", time.Hour) // accepted outcome: violation doesn't count
	addAdherence(t, st, id, "followed", "accepted", time.Hour)
	addAdherence(t, st, "lesson_deadbeef", "violated", "corrected", time.Hour) // unknown id ignored

	cands, err = Reduce(context.Background(), st)
	if err != nil || len(cands) != 1 {
		t.Fatal(err)
	}
	c := cands[0]
	if c.Violations != 2 {
		t.Errorf("violations = %d, want 2 (accepted-outcome violation must not count)", c.Violations)
	}
	if c.Reinforcements != 1 {
		t.Errorf("reinforcements = %d, want 1", c.Reinforcements)
	}
	if c.Weight <= base || c.Weight > base+reinforceCap+0.001 {
		t.Errorf("weight %f should gain a capped reinforcement over base %f", c.Weight, base)
	}
	// 2 violations = threshold → excluded from promotion, listed for demotion once promoted.
	if got := PendingPromotions(t.TempDir(), cands); len(got) != 0 {
		t.Errorf("violated-out candidate must not promote: %+v", got)
	}
}

// Demotion drops a marker; the same stale evidence can't re-promote, but a NEW
// signal after the marker re-earns eligibility.
func TestDemoteMarkerBlocksRepromotion(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	for i, sess := range []string{"s1", "s2", "s3"} {
		addSignal(t, st, trigGoImports, stratGofmt, sess, 0.9, time.Duration(i+1)*time.Hour)
	}
	cands, _ := Reduce(context.Background(), st)
	if _, err := WriteLesson(root, cands[0]); err != nil {
		t.Fatal(err)
	}
	if err := Demote(root, cands[0]); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(root, ".memcode", "lessons", cands[0].ID+"-*.md")); len(matches) != 0 {
		t.Fatal("demote must remove the lesson file")
	}
	if got := PendingPromotions(root, cands); len(got) != 0 {
		t.Errorf("demoted candidate with no new evidence must not re-promote: %+v", got)
	}
	// A fresh signal AFTER the marker re-earns.
	addSignal(t, st, trigGoImports, stratGofmt, "s4", 0.9, -time.Hour) // future-dated: strictly after marker
	cands, _ = Reduce(context.Background(), st)
	if got := PendingPromotions(root, cands); len(got) != 1 {
		t.Errorf("new evidence after demotion should re-earn promotion, got %+v", got)
	}
	// Promotion clears the marker.
	if _, err := WriteLesson(root, cands[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".memcode", "lessons", cands[0].ID+".demoted")); !os.IsNotExist(err) {
		t.Error("promotion must clear the demotion marker")
	}
}

// Provenance: evidence lines carry the episode's commit (sha7).
func TestWriteLessonEvidenceCarriesCommit(t *testing.T) {
	root := t.TempDir()
	c := Candidate{
		ID: "lesson_cafe01", Trigger: trigGoImports, Strategy: stratGofmt, Weight: 2.5,
		SignalCount: 3, SessionCount: 2,
		Evidence: []Signal{{Trigger: trigGoImports, Strategy: stratGofmt, Strength: 0.9,
			SessionID: "s1", HeadSHA: "abc1234def5678", TS: time.Now()}},
	}
	path, err := WriteLesson(root, c)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "[s1 @ abc1234]") {
		t.Errorf("evidence line must carry session @ sha7, got:\n%s", data)
	}
}

// InlineTop returns the ids of exactly the lessons it surfaced.
func TestInlineTopReturnsIDs(t *testing.T) {
	root := t.TempDir()
	c := Candidate{ID: "lesson_beef02", Trigger: trigGoImports, Strategy: stratGofmt,
		Weight: 2.5, SignalCount: 3, SessionCount: 2}
	if _, err := WriteLesson(root, c); err != nil {
		t.Fatal(err)
	}
	block, ids := InlineTop(root)
	if block == "" || len(ids) != 1 || ids[0] != "lesson_beef02" {
		t.Errorf("InlineTop = (%q, %v), want the promoted lesson's id", block, ids)
	}
}
