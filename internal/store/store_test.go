package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEventRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	id, err := s.AppendEvent(ctx, Event{
		Kind:    "assertion",
		Actor:   "user",
		Payload: json.RawMessage(`{"text":"no typescript"}`),
	})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero event id")
	}

	got, err := s.ListEvents(ctx, EventFilter{Kinds: []string{"assertion"}})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "assertion" || got[0].Actor != "user" {
		t.Fatalf("unexpected events: %+v", got)
	}
	if string(got[0].Payload) != `{"text":"no typescript"}` {
		t.Errorf("payload = %s", got[0].Payload)
	}

	// A filter that matches nothing returns empty, not an error.
	none, err := s.ListEvents(ctx, EventFilter{Kinds: []string{"commit"}})
	if err != nil || len(none) != 0 {
		t.Fatalf("expected no commit events, got %v (err %v)", none, err)
	}
}

func TestEntityAndEdgeUpsert(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// Upsert is idempotent on (kind,key) and updates attrs.
	for _, attrs := range []string{`{"v":1}`, `{"v":2}`} {
		if err := s.UpsertEntity(ctx, Entity{Kind: "subsystem", Key: "cli", Attrs: json.RawMessage(attrs)}); err != nil {
			t.Fatalf("UpsertEntity: %v", err)
		}
	}
	if err := s.UpsertEntity(ctx, Entity{Kind: "subsystem", Key: "apps/www"}); err != nil {
		t.Fatalf("UpsertEntity: %v", err)
	}

	subs, err := s.ListEntities(ctx, "subsystem")
	if err != nil {
		t.Fatalf("ListEntities: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 subsystems, got %d", len(subs))
	}

	got, ok, err := s.GetEntity(ctx, "subsystem:cli")
	if err != nil || !ok {
		t.Fatalf("GetEntity: ok=%v err=%v", ok, err)
	}
	if string(got.Attrs) != `{"v":2}` {
		t.Errorf("attrs not updated: %s", got.Attrs)
	}

	if err := s.UpsertEdge(ctx, Edge{Src: "subsystem:apps/www", Dst: "subsystem:cli", Kind: "depends_on"}); err != nil {
		t.Fatalf("UpsertEdge: %v", err)
	}
	edges, err := s.ListEdges(ctx, EdgeFilter{Src: "subsystem:apps/www"})
	if err != nil || len(edges) != 1 || edges[0].Kind != "depends_on" {
		t.Fatalf("unexpected edges: %+v (err %v)", edges, err)
	}
}

func TestObjectiveLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now().UTC()

	o := Objective{ID: "obj_1", Title: "ship v1", Status: "active", Priority: 5, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateObjective(ctx, o); err != nil {
		t.Fatalf("CreateObjective: %v", err)
	}
	o.Status = "done"
	o.UpdatedAt = now.Add(time.Hour)
	if err := s.UpdateObjective(ctx, o); err != nil {
		t.Fatalf("UpdateObjective: %v", err)
	}
	got, ok, err := s.GetObjective(ctx, "obj_1")
	if err != nil || !ok {
		t.Fatalf("GetObjective: ok=%v err=%v", ok, err)
	}
	if got.Status != "done" {
		t.Errorf("status = %s, want done", got.Status)
	}

	// Updating an objective that doesn't exist must NOT silently succeed.
	missing := Objective{ID: "obj_nope", Title: "ghost", Status: "active", Priority: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.UpdateObjective(ctx, missing); err == nil {
		t.Error("UpdateObjective on a non-existent id should error, got nil")
	}
}

func TestStateUpsert(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if err := s.PutState(ctx, State{Scope: "repo", Layer: "structural", Body: json.RawMessage(`{"n":1}`)}); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	if err := s.PutState(ctx, State{Scope: "repo", Layer: "structural", Body: json.RawMessage(`{"n":2}`)}); err != nil {
		t.Fatalf("PutState (update): %v", err)
	}

	got, ok, err := s.GetState(ctx, "repo", "structural")
	if err != nil || !ok {
		t.Fatalf("GetState: ok=%v err=%v", ok, err)
	}
	if string(got.Body) != `{"n":2}` {
		t.Errorf("body = %s, want {\"n\":2}", got.Body)
	}
	if got.RefreshedAt.IsZero() {
		t.Error("expected refreshed_at to be set")
	}

	// Missing state is (zero, false, nil), not an error.
	if _, ok, err := s.GetState(ctx, "repo", "doctrine"); err != nil || ok {
		t.Fatalf("expected missing doctrine state, ok=%v err=%v", ok, err)
	}
}

// TestRunInTxCommits: a successful closure must persist all claims. ClearClaims +
// two AddClaims inside a RunInTx → ListClaims shows exactly those two.
func TestRunInTxCommits(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// Seed an initial claim so we can prove ClearClaims ran inside the tx.
	if err := s.AddClaim(ctx, Claim{ID: "old", Type: "doctrine", Text: "old claim", Status: "stale"}); err != nil {
		t.Fatalf("seed AddClaim: %v", err)
	}

	err := s.RunInTx(ctx, func(tx Tx) error {
		if err := tx.ClearClaims(ctx); err != nil {
			return err
		}
		if err := tx.AddClaim(ctx, Claim{ID: "a", Type: "command", Text: "claim A", Status: "current"}); err != nil {
			return err
		}
		if err := tx.AddClaim(ctx, Claim{ID: "b", Type: "doctrine", Text: "claim B", Status: "current"}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}

	got, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatalf("ListClaims: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 claims after commit, got %d: %+v", len(got), got)
	}
	// The old claim must be gone (ClearClaims ran inside the tx).
	for _, c := range got {
		if c.ID == "old" {
			t.Error("old claim should have been cleared by the tx")
		}
	}
}

// TestRunInTxAtomicity: a closure that returns an error mid-loop must roll back —
// neither the ClearClaims nor the partially-inserted claims should persist. The
// old claim set must survive intact.
func TestRunInTxAtomicity(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// Seed an initial claim that the rollback must preserve.
	if err := s.AddClaim(ctx, Claim{ID: "survivor", Type: "doctrine", Text: "survives rollback", Status: "current"}); err != nil {
		t.Fatalf("seed AddClaim: %v", err)
	}

	// A closure that clears then inserts one, then fails on the second.
	sentinel := errors.New("boom")
	err := s.RunInTx(ctx, func(tx Tx) error {
		if err := tx.ClearClaims(ctx); err != nil {
			return err
		}
		if err := tx.AddClaim(ctx, Claim{ID: "partial", Type: "command", Text: "partial", Status: "current"}); err != nil {
			return err
		}
		return sentinel // simulate a mid-loop failure
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error from RunInTx, got: %v", err)
	}

	got, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatalf("ListClaims: %v", err)
	}
	// The rollback must restore the old claim set: survivor present, partial gone.
	if len(got) != 1 {
		t.Fatalf("expected 1 claim (survivor) after rollback, got %d: %+v", len(got), got)
	}
	if got[0].ID != "survivor" {
		t.Errorf("expected survivor claim, got %q", got[0].ID)
	}
}

func TestPreferenceCandidateCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now().UTC().Truncate(time.Second)

	seed := PreferenceCandidate{
		ID:           "prefcand_abc1",
		Axis:         "workflow",
		Text:         "always commit, deploy, rebuild after gateway changes",
		Scope:        ".",
		Weight:       2.4,
		SignalCount:  3,
		SessionCount: 2,
		FirstSeen:    now.Add(-48 * time.Hour),
		LastSeen:     now,
		Status:       "candidate",
		Evidence:     json.RawMessage(`[{"session":"s1","text":"always commit, deploy, rebuild"}]`),
	}
	if err := s.AddPreferenceCandidate(ctx, seed); err != nil {
		t.Fatalf("AddPreferenceCandidate: %v", err)
	}

	got, err := s.ListPreferenceCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPreferenceCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	c := got[0]
	if c.ID != seed.ID || c.Axis != seed.Axis || c.Text != seed.Text || c.Scope != seed.Scope {
		t.Errorf("identity fields mismatch: got %+v", c)
	}
	if c.Weight != seed.Weight || c.SignalCount != seed.SignalCount || c.SessionCount != seed.SessionCount {
		t.Errorf("numeric fields mismatch: got %+v", c)
	}
	if !c.FirstSeen.Equal(seed.FirstSeen) || !c.LastSeen.Equal(seed.LastSeen) {
		t.Errorf("time fields mismatch: first=%s want=%s last=%s want=%s", c.FirstSeen, seed.FirstSeen, c.LastSeen, seed.LastSeen)
	}
	if c.Status != "candidate" {
		t.Errorf("status = %q, want candidate", c.Status)
	}
	if c.ConfirmedPath != "" {
		t.Errorf("confirmed_path = %q, want empty", c.ConfirmedPath)
	}
	if string(c.Evidence) != string(seed.Evidence) {
		t.Errorf("evidence = %s, want %s", c.Evidence, seed.Evidence)
	}

	// Promote: UpdatePreferenceCandidateStatus with a confirmedPath.
	wantPath := "/repo/.memcode/prefs/prefcand_abc1-always-commit-deploy-rebuild.md"
	if err := s.UpdatePreferenceCandidateStatus(ctx, seed.ID, "confirmed", wantPath, 3.1); err != nil {
		t.Fatalf("UpdatePreferenceCandidateStatus (promote): %v", err)
	}
	got, _ = s.ListPreferenceCandidates(ctx)
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate after promote, got %d", len(got))
	}
	if got[0].Status != "confirmed" {
		t.Errorf("status = %q, want confirmed", got[0].Status)
	}
	if got[0].ConfirmedPath != wantPath {
		t.Errorf("confirmed_path = %q, want %q", got[0].ConfirmedPath, wantPath)
	}
	if got[0].Weight != 3.1 {
		t.Errorf("weight = %v, want 3.1", got[0].Weight)
	}

	// Demote: UpdatePreferenceCandidateStatus with an empty confirmedPath must
	// NULL out confirmed_path (the demotion path passes "").
	if err := s.UpdatePreferenceCandidateStatus(ctx, seed.ID, "demoted", "", 0); err != nil {
		t.Fatalf("UpdatePreferenceCandidateStatus (demote): %v", err)
	}
	got, _ = s.ListPreferenceCandidates(ctx)
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate after demote, got %d", len(got))
	}
	if got[0].Status != "demoted" {
		t.Errorf("status = %q, want demoted", got[0].Status)
	}
	if got[0].ConfirmedPath != "" {
		t.Errorf("confirmed_path = %q, want empty after demote", got[0].ConfirmedPath)
	}

	// ClearPreferenceCandidates wipes the projection.
	if err := s.ClearPreferenceCandidates(ctx); err != nil {
		t.Fatalf("ClearPreferenceCandidates: %v", err)
	}
	got, _ = s.ListPreferenceCandidates(ctx)
	if len(got) != 0 {
		t.Fatalf("expected 0 candidates after clear, got %d", len(got))
	}
}

// TestRunInTxPreferenceAtomicity: a RunInTx that clears + inserts a candidate
// and then returns an error must roll back — the seeded candidate survives.
func TestRunInTxPreferenceAtomicity(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now().UTC()

	seed := PreferenceCandidate{
		ID: "prefcand_survivor", Axis: "style", Text: "use pnpm, never npm",
		Weight: 2.0, SignalCount: 3, SessionCount: 2,
		FirstSeen: now.Add(-24 * time.Hour), LastSeen: now, Status: "candidate",
	}
	if err := s.AddPreferenceCandidate(ctx, seed); err != nil {
		t.Fatalf("seed AddPreferenceCandidate: %v", err)
	}

	sentinel := errors.New("boom")
	err := s.RunInTx(ctx, func(tx Tx) error {
		if err := tx.ClearPreferenceCandidates(ctx); err != nil {
			return err
		}
		if err := tx.AddPreferenceCandidate(ctx, PreferenceCandidate{
			ID: "prefcand_partial", Axis: "style", Text: "partial",
			Weight: 0.5, SignalCount: 1, SessionCount: 1,
			FirstSeen: now, LastSeen: now, Status: "candidate",
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got: %v", err)
	}

	got, err := s.ListPreferenceCandidates(ctx)
	if err != nil {
		t.Fatalf("ListPreferenceCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate (survivor) after rollback, got %d: %+v", len(got), got)
	}
	if got[0].ID != "prefcand_survivor" {
		t.Errorf("expected survivor candidate, got %q", got[0].ID)
	}
}
