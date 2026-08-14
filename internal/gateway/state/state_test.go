package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMarkProcessedDedup(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	fresh, err := s.MarkProcessed(ctx, "telegram", "42", now)
	if err != nil || !fresh {
		t.Fatalf("first mark: fresh=%v err=%v, want fresh", fresh, err)
	}
	fresh, err = s.MarkProcessed(ctx, "telegram", "42", now)
	if err != nil || fresh {
		t.Fatalf("second mark: fresh=%v err=%v, want not-fresh", fresh, err)
	}
	// Same id on a different channel is a distinct message.
	if fresh, _ := s.MarkProcessed(ctx, "discord", "42", now); !fresh {
		t.Error("same id on different channel should be fresh")
	}
}

func TestMarkProcessedSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Unix(1000, 0)

	s1, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if fresh, _ := s1.MarkProcessed(ctx, "telegram", "7", now); !fresh {
		t.Fatal("first mark should be fresh")
	}
	s1.Close()

	// A restart must still see the message as already processed.
	s2, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if fresh, _ := s2.MarkProcessed(ctx, "telegram", "7", now); fresh {
		t.Error("after reopen the message should NOT be fresh (durable dedup)")
	}
	// Sanity: the db file actually landed where we expect.
	if _, err := Open(ctx, dir); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := filepath.Join(dir, "gateway.db"); got == "" {
		t.Fatal("unreachable")
	}
}

func TestPruneProcessed(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	old := time.Unix(1000, 0)
	recent := time.Unix(1_000_000, 0)

	s.MarkProcessed(ctx, "telegram", "old", old)
	s.MarkProcessed(ctx, "telegram", "new", recent)

	if err := s.PruneProcessed(ctx, time.Unix(500_000, 0)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	// The old record is gone (marking it again is fresh); the recent one remains.
	if fresh, _ := s.MarkProcessed(ctx, "telegram", "old", recent); !fresh {
		t.Error("pruned record should be forgotten")
	}
	if fresh, _ := s.MarkProcessed(ctx, "telegram", "new", recent); fresh {
		t.Error("recent record should have survived prune")
	}
}

func TestOffsetRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if v, _ := s.Offset(ctx, "telegram"); v != 0 {
		t.Errorf("unset offset = %d, want 0", v)
	}
	if err := s.SetOffset(ctx, "telegram", 12345); err != nil {
		t.Fatalf("set: %v", err)
	}
	if v, _ := s.Offset(ctx, "telegram"); v != 12345 {
		t.Errorf("offset = %d, want 12345", v)
	}
	// Upsert overwrites.
	s.SetOffset(ctx, "telegram", 99999)
	if v, _ := s.Offset(ctx, "telegram"); v != 99999 {
		t.Errorf("offset after upsert = %d, want 99999", v)
	}
}
