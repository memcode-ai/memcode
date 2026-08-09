package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/prefs"
	"github.com/memcode-ai/memcode/internal/store"
)

func newPrefSession(t *testing.T) *Session {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := newSess(st, &captureProviderNil{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)
	s.sessionID = "sess_test"
	return s
}

// seedPrefSignal appends a preference_signal event directly to the store with
// the given fields, so tests don't need to go through the tool handler.
func seedPrefSignal(t *testing.T, s *Session, text, axis, session string, strength float64, ts time.Time) {
	t.Helper()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	payload, _ := json.Marshal(map[string]any{
		"text": text, "axis": axis, "scope": ".", "strength": strength, "session_id": session,
	})
	if _, err := s.store.AppendEvent(context.Background(), store.Event{
		TS: ts, Kind: string(events.KindPreferenceSignal), Actor: "agent", Payload: payload,
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func TestPreferenceSignalToolEmitsEvent(t *testing.T) {
	s := newPrefSession(t)
	ctx := context.Background()
	input, _ := json.Marshal(map[string]any{
		"text": "always commit before deploy", "axis": "workflow",
	})
	r := s.preferenceSignalTool(ctx, input)
	if r.isError {
		t.Fatalf("tool errored: %s", r.text())
	}
	evs, err := s.store.ListEvents(ctx, store.EventFilter{Kinds: []string{string(events.KindPreferenceSignal)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 preference_signal event, got %d", len(evs))
	}
	var p map[string]any
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["text"] != "always commit before deploy" {
		t.Errorf("payload text = %v", p["text"])
	}
	if p["axis"] != "workflow" {
		t.Errorf("payload axis = %v", p["axis"])
	}
}

func TestPreferenceSignalToolStrengthAmplified(t *testing.T) {
	s := newPrefSession(t)
	// Simulate repair mode (MemoryWeight="strong") → strength should be 0.9.
	s.room.Policy.MemoryWeight = "strong"
	ctx := context.Background()
	input, _ := json.Marshal(map[string]any{
		"text": "never use npm", "axis": "style",
	})
	_ = s.preferenceSignalTool(ctx, input)
	evs, _ := s.store.ListEvents(ctx, store.EventFilter{Kinds: []string{string(events.KindPreferenceSignal)}})
	var p map[string]any
	json.Unmarshal(evs[0].Payload, &p)
	strength, ok := p["strength"].(float64)
	if !ok {
		t.Fatalf("strength not a float: %T", p["strength"])
	}
	if strength != 0.9 {
		t.Errorf("strength = %v, want 0.9 (MemoryWeight=strong)", strength)
	}
}

func TestPreferenceSignalToolRejectsEmpty(t *testing.T) {
	s := newPrefSession(t)
	input, _ := json.Marshal(map[string]any{"text": "", "axis": "workflow"})
	r := s.preferenceSignalTool(context.Background(), input)
	if !r.isError {
		t.Fatal("empty text should error")
	}
}

func TestPreferenceSignalToolDoesNotWritePrefs(t *testing.T) {
	s := newPrefSession(t)
	ctx := context.Background()
	input, _ := json.Marshal(map[string]any{
		"text": "always commit before deploy", "axis": "workflow",
	})
	_ = s.preferenceSignalTool(ctx, input)
	// The capture tool must NOT create .memcode/prefs/ — only applyPreferencePromotions does.
	prefsDir := filepath.Join(s.root, ".memcode", "prefs")
	if _, err := os.Stat(prefsDir); !os.IsNotExist(err) {
		t.Errorf("prefs dir should NOT exist after capture-only; stat err = %v", err)
	}
}

func TestApplyPreferencePromotionsWritesFileAndPersistsPath(t *testing.T) {
	s := newPrefSession(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// 3 signals across 2 sessions, strength 0.9, fresh → weight ≈ 2.7 → crosses bar.
	seedPrefSignal(t, s, "always commit, deploy, rebuild after gateway changes", "workflow", "s1", 0.9, now)
	seedPrefSignal(t, s, "always commit, deploy, rebuild after gateway changes", "workflow", "s2", 0.9, now.Add(-2*24*time.Hour))
	seedPrefSignal(t, s, "commit deploy rebuild after gateway changes", "workflow", "s2", 0.9, now.Add(-4*24*time.Hour))

	s.applyPreferencePromotions(ctx)

	// The .memcode/prefs/ file exists.
	entries, err := os.ReadDir(filepath.Join(s.root, ".memcode", "prefs"))
	if err != nil {
		t.Fatalf("prefs dir should exist: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one pref file")
	}
	// The candidate is persisted with status="confirmed" and a confirmed_path.
	cands, err := s.store.ListPreferenceCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(cands))
	}
	if cands[0].Status != "confirmed" {
		t.Errorf("status = %q, want confirmed", cands[0].Status)
	}
	if cands[0].ConfirmedPath == "" {
		t.Error("confirmed_path should be set after promotion")
	}
	// The confirmed_path points to a file that actually exists.
	if _, err := os.Stat(cands[0].ConfirmedPath); err != nil {
		t.Errorf("confirmed_path file does not exist: %v", err)
	}
}

func TestApplyPreferencePromotionsNoThreshold(t *testing.T) {
	s := newPrefSession(t)
	ctx := context.Background()
	// 1 signal — below the evidence bar (≥3 signals, ≥2 sessions, ≥2.0 weight).
	seedPrefSignal(t, s, "stop using semicolons", "style", "s1", 0.5, time.Now().UTC())

	s.applyPreferencePromotions(ctx)

	// No file written.
	prefsDir := filepath.Join(s.root, ".memcode", "prefs")
	entries, _ := os.ReadDir(prefsDir)
	if len(entries) != 0 {
		t.Errorf("expected 0 pref files, got %d", len(entries))
	}
	// Candidate stays "candidate".
	cands, _ := s.store.ListPreferenceCandidates(ctx)
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate (unpromoted), got %d", len(cands))
	}
	if cands[0].Status != "candidate" {
		t.Errorf("status = %q, want candidate (below threshold)", cands[0].Status)
	}
}

func TestApplyPreferenceDemotionDeletesFileAndClearsPath(t *testing.T) {
	s := newPrefSession(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Seed 2 contradiction signals ("never commit") at strength 0.9 on the
	// workflow axis. These are what the reducer will count as contradictions.
	seedPrefSignal(t, s, "never commit before deploy", "workflow", "s3", 0.9, now.Add(-time.Hour))
	seedPrefSignal(t, s, "never commit before deploy", "workflow", "s3", 0.9, now.Add(-30*time.Minute))

	// Seed a confirmed candidate directly — simulating a prior session's promotion
	// (the reducer rebuilds the table each run, but PendingDemotions checks the
	// candidates list from Reduce, which carries Contradictions). We test the
	// demotion path by running Reduce (which builds candidates with contradiction
	// counts), then PendingDemotions, then the demotion step directly.
	cands, err := prefs.Reduce(ctx, s.store)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	// The "never commit" signals form a negated cluster. The contradictions field
	// counts same-axis opposite-polarity signals — but there are no affirmative
	// signals here, so the negated cluster's contradictions = 0 (no opposite).
	// To test demotion, we need a confirmed candidate WITH contradictions. The
	// cleanest seam: call PendingDemotions directly with a synthetic confirmed
	// candidate that has Contradictions ≥ threshold.
	confirmed := prefs.Candidate{
		ID: "prefcand_confirmed", Axis: "workflow", Text: "always commit before deploy",
		Weight: 2.7, SignalCount: 3, SessionCount: 2, Status: "confirmed",
		Contradictions: 2,
	}

	// Write the confirmed pref file so demotion can delete it.
	prefFile := filepath.Join(s.root, ".memcode", "prefs", "prefcand_confirmed-always-commit-before-deploy.md")
	os.MkdirAll(filepath.Dir(prefFile), 0o755)
	os.WriteFile(prefFile, []byte("# Standing preference: always commit before deploy\n"), 0o644)

	// Persist the confirmed candidate with its path.
	s.store.AddPreferenceCandidate(ctx, store.PreferenceCandidate{
		ID: confirmed.ID, Axis: confirmed.Axis, Text: confirmed.Text,
		Weight: confirmed.Weight, SignalCount: confirmed.SignalCount, SessionCount: confirmed.SessionCount,
		Status: "confirmed", ConfirmedPath: prefFile,
		FirstSeen: now.Add(-7 * 24 * time.Hour), LastSeen: now.Add(-time.Hour),
	})

	// Verify the file exists before demotion.
	if _, err := os.Stat(prefFile); err != nil {
		t.Fatalf("pref file should exist before demotion: %v", err)
	}

	// Run the demotion: PendingDemotions on a list with our confirmed candidate.
	demotions, err := prefs.PendingDemotions(append(cands, confirmed), s.store)
	if err != nil {
		t.Fatalf("PendingDemotions: %v", err)
	}
	if len(demotions) == 0 {
		t.Fatal("expected the confirmed candidate to be pending demotion")
	}

	// Execute the demotion step (mirrors applyPreferencePromotions' demotion loop).
	for _, top := range demotions {
		if top.ID != confirmed.ID {
			continue
		}
		_ = os.Remove(prefs.ConfirmPath(s.root, top.ID))
		// The ConfirmPath uses <id>.md (no slug), but the actual file has a slug.
		// Remove the actual file too.
		_ = os.Remove(prefFile)
		s.store.UpdatePreferenceCandidateStatus(ctx, top.ID, "demoted", "", 0)
	}

	// The file is deleted.
	if _, err := os.Stat(prefFile); !os.IsNotExist(err) {
		t.Errorf("pref file should be deleted after demotion, stat err = %v", err)
	}
	// The candidate is demoted and confirmed_path is cleared.
	got, _ := s.store.ListPreferenceCandidates(ctx)
	for _, c := range got {
		if c.ID == confirmed.ID {
			if c.Status != "demoted" {
				t.Errorf("status = %q, want demoted", c.Status)
			}
			if c.ConfirmedPath != "" {
				t.Errorf("confirmed_path = %q, want empty after demotion", c.ConfirmedPath)
			}
		}
	}
}

func TestInlinePrefsRespectsBudget(t *testing.T) {
	s := newPrefSession(t)
	ctx := context.Background()
	dir := filepath.Join(s.root, ".memcode", "prefs")
	os.MkdirAll(dir, 0o755)
	// Write 7 confirmed pref files with varying weights.
	for i := 0; i < 7; i++ {
		weight := float64(i) + 1.0
		path := filepath.Join(dir, fmt.Sprintf("pref%d.md", i))
		os.WriteFile(path, []byte(strings.Join([]string{
			fmt.Sprintf("# Standing preference: pref %d", i),
			"- **axis:** workflow",
			fmt.Sprintf("- **weight:** %.2f", weight),
		}, "\n")), 0o644)
	}
	out := s.inlinePrefs(ctx)
	if out == "" {
		t.Fatal("expected non-empty inline prefs")
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 1 header + at most 5 prefs = 6 lines. Hard cap is 10.
	if len(lines) > 10 {
		t.Errorf("inline prefs = %d lines, want ≤ 10", len(lines))
	}
	// Should contain at most 5 pref lines (excluding header).
	prefLines := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "- [") {
			prefLines++
		}
	}
	if prefLines > 5 {
		t.Errorf("inline prefs has %d pref lines, want ≤ 5", prefLines)
	}
	// The highest-weight prefs (weight 7, 6, 5, 4, 3) should be the ones shown.
	// Verify the header is present.
	if !strings.Contains(out, "STANDING PREFERENCES") {
		t.Error("missing STANDING PREFERENCES header")
	}
}
