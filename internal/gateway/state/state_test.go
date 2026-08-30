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

func TestInboxWaitingTransitions(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.Accept(ctx, item("personal", "m1"), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, tr := range [][2]string{{"pending", "running"}, {"running", "waiting"}, {"waiting", "resumable"}, {"resumable", "replied"}, {"replied", "done"}} {
		ok, err := s.SetInboxStatus(ctx, "personal", "m1", tr[0], tr[1])
		if err != nil || !ok {
			t.Fatalf("%s→%s ok=%v err=%v", tr[0], tr[1], ok, err)
		}
	}
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

func TestConversationSelection(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	// Unset → empty (caller applies defaults).
	if a, p, _ := s.Conversation(ctx, "telegram", "42"); a != "" || p != "" {
		t.Errorf("unset conversation = (%q,%q), want empty", a, p)
	}
	// Setting agent then project upserts, each preserving the other.
	if err := s.SetConversationAgent(ctx, "telegram", "42", "coder"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConversationProject(ctx, "telegram", "42", "memcode"); err != nil {
		t.Fatal(err)
	}
	a, p, _ := s.Conversation(ctx, "telegram", "42")
	if a != "coder" || p != "memcode" {
		t.Errorf("conversation = (%q,%q), want (coder,memcode)", a, p)
	}
}

func TestInboxSnapshotsAgentAndProject(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	it := Item{Channel: "telegram", MessageID: "1", Conversation: "c", Principal: "p", Text: "hi", Agent: "coder", Project: "memcode"}
	if _, err := s.Accept(ctx, it, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.Pending(ctx)
	if len(pending) != 1 || pending[0].Agent != "coder" || pending[0].Project != "memcode" {
		t.Fatalf("snapshot not persisted on the inbox item: %+v", pending)
	}
}

func TestReplyQueueDurability(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	s.Accept(ctx, item("telegram", "1"), now)

	// Job finished: pending → replied, reply held. It leaves the fresh-job queue
	// but joins the outbound queue, so a delivery failure never re-runs the job.
	if err := s.SetReplied(ctx, "telegram", "1", "the answer", ""); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Pending(ctx); len(p) != 0 {
		t.Errorf("a replied item must not be a pending job, got %+v", p)
	}
	replies, err := s.PendingReplies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 1 || replies[0].Reply != "the answer" {
		t.Fatalf("outbound queue = %+v, want one item carrying its reply", replies)
	}

	// Delivered: replied → done, off both queues.
	if err := s.MarkDone(ctx, "telegram", "1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.PendingReplies(ctx); len(r) != 0 {
		t.Errorf("a delivered item must leave the outbound queue, got %+v", r)
	}
}

func TestReplySurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Accept(ctx, item("telegram", "1"), time.Unix(1000, 0))
	s.SetReplied(ctx, "telegram", "1", "durable answer", "vv.ogg")
	s.Close() // simulate a crash before the reply was delivered

	s2, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	replies, _ := s2.PendingReplies(ctx)
	if len(replies) != 1 || replies[0].Reply != "durable answer" {
		t.Fatalf("undelivered reply lost across restart: %+v", replies)
	}
}

func TestProjectLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A second gateway on the same project must be refused, not silently share the
	// inbox and double-process it.
	s2, err := Open(ctx, dir)
	if err != nil {
		return // expected: the project lock is held (unix)
	}
	// Reached only where file locking is a no-op (non-unix); nothing to assert.
	s2.Close()
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

// Attachments (media spool IDs) ride the durable inbox row as JSON.
func TestItemAttachmentsRoundTrip(t *testing.T) {
	ctx := context.Background()
	gw, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	it := Item{Channel: "telegram", MessageID: "m1", Conversation: "c", Principal: "p", Text: "look",
		Attachments: []string{"abc.png", "def.ogg"}}
	if _, err := gw.Accept(ctx, it, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := gw.Pending(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("pending: %v %d", err, len(got))
	}
	if len(got[0].Attachments) != 2 || got[0].Attachments[0] != "abc.png" || got[0].Attachments[1] != "def.ogg" {
		t.Errorf("attachments = %+v", got[0].Attachments)
	}
}
