package autonomy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOpenInitializesHomeAndSchema(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), "agent")
	s, err := Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, path := range []string{"personal.db", "policies", "workspace/generated", "workspace/scratch", "runs", "workers", ".memcode/jobs", ".memcode/sessions"} {
		if _, err := os.Stat(filepath.Join(home, path)); err != nil {
			t.Errorf("missing %s: %v", path, err)
		}
	}
	for _, table := range []string{"objectives", "subgoals", "runs", "triggers", "policies", "resources", "facts", "actions", "generated_items", "notifications"} {
		var name string
		if err := s.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal mode=%q err=%v", mode, err)
	}
}

func TestObjectiveAndDomainNeutralRecordsPersist(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	s, err := Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateObjective(ctx, Objective{ID: "o1", Description: "Maintain an arbitrary long-lived outcome", Status: "active", Priority: 3}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetObjective(ctx, "o1")
	if err != nil || !ok || got.Description == "" || got.Priority != 3 {
		t.Fatalf("objective=%+v ok=%v err=%v", got, ok, err)
	}
	if err := s.UpsertSubgoal(ctx, Subgoal{ID: "g1", ObjectiveID: "o1", Description: "observe state", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, Run{ID: "r1", ObjectiveID: "o1", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTrigger(ctx, Trigger{ID: "t1", ObjectiveID: "o1", Kind: "manual", Spec: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertPolicy(ctx, Policy{ID: "p1", ObjectiveID: "o1", Version: 1, Document: json.RawMessage(`{}`), Hash: "h", Status: "draft"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertResource(ctx, Resource{ID: "res1", ObjectiveID: "o1", Type: "filesystem", Locator: "/tmp/x", AccessMode: "read", AuthorizationSource: "user", PolicyHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertFact(ctx, Fact{ID: "f1", ObjectiveID: "o1", Key: "environment.state", Value: json.RawMessage(`{}`), Source: "observation"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertNotification(ctx, Notification{ID: "n1", ObjectiveID: "o1", Kind: "info"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, table := range []string{"subgoals", "runs", "triggers", "policies", "resources", "facts", "notifications"} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s count=%d err=%v", table, n, err)
		}
	}
}

func TestPersistentTriggerClaim(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-time.Minute)
	// A self-scheduled wake: one instant, set by the agent from inside a run.
	if err := s.CreateTrigger(ctx, Trigger{ID: "t1", ObjectiveID: "o1", Kind: "next_wake", Spec: due.Format(time.RFC3339), NextDueAt: &due}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.ClaimDueTrigger(ctx, "t1", now)
	if err != nil || !ok || got.LastFiredAt == nil {
		t.Fatalf("trigger=%+v ok=%v err=%v", got, ok, err)
	}
	// The claim is atomic — a second gateway process racing on the same row
	// must lose rather than double-firing the wake.
	if _, ok, err := s.ClaimDueTrigger(ctx, "t1", now); err != nil || ok {
		t.Fatalf("duplicate claim ok=%v err=%v", ok, err)
	}
	triggers, err := s.ListTriggers(ctx)
	if err != nil || len(triggers) != 1 {
		t.Fatalf("triggers=%+v err=%v", triggers, err)
	}
	// Firing completes it: a one-instant wake never reschedules itself.
	if triggers[0].Status != "completed" || triggers[0].NextDueAt != nil {
		t.Fatalf("expected a completed wake with no next due, got %+v", triggers[0])
	}
}

func TestNextDueKinds(t *testing.T) {
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ kind, spec string }{{"manual", ""}, {"one_shot", "2026-08-30T13:00:00Z"}, {"next_wake", "2026-08-30T13:00:00Z"}} {
		if _, err := NextDue(tc.kind, tc.spec, now); err != nil {
			t.Errorf("%s: %v", tc.kind, err)
		}
	}
	// Recurring kinds are deliberately NOT understood here: a second cron
	// parser in this package is what let the two schedulers drift. Human
	// cadence is a gateway schedule (gw_schedule), not a trigger row.
	for _, kind := range []string{"interval", "cron"} {
		if _, err := NextDue(kind, "5m", now); err == nil {
			t.Errorf("%s accepted — recurring cadence must go through gwconfig, not a second parser here", kind)
		}
	}
}

func TestStatusRecoveryRevocationAndUncertainResolution(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateObjective(ctx, Objective{ID: "o1", Description: "arbitrary", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, Run{ID: "r1", ObjectiveID: "o1", Status: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertResource(ctx, Resource{ID: "res1", ObjectiveID: "o1", Type: "filesystem", Locator: "/tmp", AccessMode: "read", AuthorizationSource: "user", PolicyHash: "h", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.ReserveAction(ctx, ActionIntent{ID: "a1", ObjectiveID: "o1", Kind: "write", Consequence: LocalMutation, PolicyHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAction(ctx, a.ID, ActionUncertain, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveUncertainAction(ctx, "a1", ActionFailed); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeResources(ctx, "o1"); err != nil {
		t.Fatal(err)
	}
	runs, err := s.RecoverableRuns(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	summary, err := s.StatusSummary(ctx)
	if err != nil || summary["objectives"] != 1 {
		t.Fatalf("summary=%v err=%v", summary, err)
	}
}

func TestConcurrentWALAccess(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	a, err := Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			errs <- s.CreateObjective(ctx, Objective{ID: string(rune('a' + i)), Description: "concurrent", Status: "active"})
		}(i, s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}
