package state

import (
	"context"
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

func item(channel, id string) Item {
	return Item{Channel: channel, MessageID: id, Conversation: "c", Principal: "p", Text: "hi"}
}

func TestAcceptDedup(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	if fresh, err := s.Accept(ctx, item("telegram", "42"), now); err != nil || !fresh {
		t.Fatalf("first accept: fresh=%v err=%v, want fresh", fresh, err)
	}
	if fresh, err := s.Accept(ctx, item("telegram", "42"), now); err != nil || fresh {
		t.Fatalf("second accept: fresh=%v err=%v, want not-fresh", fresh, err)
	}
	if fresh, _ := s.Accept(ctx, item("discord", "42"), now); !fresh {
		t.Error("same id on a different channel should be fresh")
	}
}

func TestPendingAndDone(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	s.Accept(ctx, item("telegram", "1"), now)
	s.Accept(ctx, item("telegram", "2"), now.Add(time.Second))

	pending, err := s.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].MessageID != "1" || pending[1].MessageID != "2" {
		t.Fatalf("pending not oldest-first: %+v", pending)
	}

	if err := s.MarkDone(ctx, "telegram", "1"); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.Pending(ctx)
	if len(pending) != 1 || pending[0].MessageID != "2" {
		t.Fatalf("after done, pending = %+v", pending)
	}
}

func TestPendingSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Unix(1000, 0)

	s1, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	s1.Accept(ctx, item("telegram", "7"), now)
	s1.Close()

	// A crash-and-restart must still see the unprocessed message as pending.
	s2, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	pending, _ := s2.Pending(ctx)
	if len(pending) != 1 || pending[0].MessageID != "7" {
		t.Errorf("pending after reopen = %+v, want the unprocessed item", pending)
	}
}

func TestPruneDone(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	old := time.Unix(1000, 0)
	recent := time.Unix(1_000_000, 0)

	s.Accept(ctx, item("telegram", "old"), old)
	s.Accept(ctx, item("telegram", "new"), recent)
	s.MarkDone(ctx, "telegram", "old")
	s.MarkDone(ctx, "telegram", "new")

	if err := s.PruneDone(ctx, time.Unix(500_000, 0)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	// The old done row is forgotten (re-accepting it is fresh); the recent one is
	// still there (re-accept not fresh).
	if fresh, _ := s.Accept(ctx, item("telegram", "old"), recent); !fresh {
		t.Error("pruned row should be forgotten")
	}
	if fresh, _ := s.Accept(ctx, item("telegram", "new"), recent); fresh {
		t.Error("recent done row should have survived prune")
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
	s.SetOffset(ctx, "telegram", 99999)
	if v, _ := s.Offset(ctx, "telegram"); v != 99999 {
		t.Errorf("offset after upsert = %d, want 99999", v)
	}
}
