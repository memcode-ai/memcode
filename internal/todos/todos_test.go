package todos

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/store"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestFromTitlesPromotesFirst(t *testing.T) {
	l := FromTitles([]string{"a", "  ", "b", "c"})
	if len(l) != 3 {
		t.Fatalf("blank title not dropped: %+v", l)
	}
	if l[0].Status != StatusActive {
		t.Errorf("first item should be active, got %q", l[0].Status)
	}
	for _, it := range l[1:] {
		if it.Status != StatusPending {
			t.Errorf("rest should be pending, got %q", it.Status)
		}
	}
}

func TestNormalizeOneActive(t *testing.T) {
	l := Normalize(List{
		{Title: "a", Status: StatusActive},
		{Title: "b", Status: StatusActive}, // second active demoted
		{Title: "c", Status: "bogus"},      // unknown → pending
	})
	if ActiveIndex(l) != 0 {
		t.Fatalf("expected single active at 0, got %d (%+v)", ActiveIndex(l), l)
	}
	if l[2].Status != StatusPending {
		t.Errorf("bogus status should normalize to pending, got %q", l[2].Status)
	}
}

func TestNormalizePromotesWhenNoneActive(t *testing.T) {
	l := Normalize(List{
		{Title: "a", Status: StatusDone},
		{Title: "b", Status: StatusPending},
	})
	if l[1].Status != StatusActive {
		t.Errorf("expected b promoted to active, got %q", l[1].Status)
	}
}

func TestAdvanceWalksAndCompletes(t *testing.T) {
	l := FromTitles([]string{"a", "b"})
	l = Advance(l) // a done, b active
	if l[0].Status != StatusDone || l[1].Status != StatusActive {
		t.Fatalf("after first advance: %+v", l)
	}
	l = Advance(l) // b done, nothing left
	if !l.AllSettled() {
		t.Fatalf("expected all settled, got %+v", l)
	}
	if ActiveIndex(l) != -1 {
		t.Errorf("no item should be active when all done")
	}
}

func TestMarkDoneAndBlock(t *testing.T) {
	l := FromTitles([]string{"a", "b", "c"}) // a active
	l = MarkBlockedAt(l, 1)                  // block a (the active one) by index
	if l[0].Status != StatusBlocked {
		t.Fatalf("item 1 should be blocked: %+v", l)
	}
	// Blocking the active item promotes the next pending to active.
	if l[1].Status != StatusActive {
		t.Fatalf("b should be promoted active after a blocked: %+v", l)
	}
	l = MarkDoneAt(l, 0) // 0 → the active item (b)
	if l[1].Status != StatusDone || l[2].Status != StatusActive {
		t.Fatalf("done-active should complete b and promote c: %+v", l)
	}
}

func TestStartSwitchesFocus(t *testing.T) {
	l := FromTitles([]string{"a", "b", "c"}) // a active
	l = StartAt(l, 3)                        // focus item 3
	if l[0].Status != StatusPending {
		t.Errorf("a should be demoted to pending, got %q", l[0].Status)
	}
	if l[2].Status != StatusActive {
		t.Errorf("c should be active, got %q", l[2].Status)
	}
	if ActiveIndex(l) != 2 {
		t.Errorf("exactly one active expected at 2, got %d", ActiveIndex(l))
	}
	// start with no index begins the next pending.
	l2 := List{{Title: "x", Status: StatusDone}, {Title: "y", Status: StatusPending}}
	l2 = StartAt(l2, 0)
	if l2[1].Status != StatusActive {
		t.Errorf("start(0) should activate next pending, got %q", l2[1].Status)
	}
}

func TestSkipAdvances(t *testing.T) {
	l := FromTitles([]string{"a", "b"}) // a active
	l = MarkSkippedAt(l, 0)             // skip active (a)
	if l[0].Status != StatusSkipped {
		t.Fatalf("a should be skipped: %+v", l)
	}
	if l[1].Status != StatusActive {
		t.Fatalf("b should be promoted active: %+v", l)
	}
	l = MarkSkippedAt(l, 0) // skip b too
	if !l.AllSettled() {
		t.Errorf("all-skipped should count as settled: %+v", l)
	}
}

func TestRenderWindowCollapsesLongList(t *testing.T) {
	l := make(List, 0, 20)
	for i := 0; i < 20; i++ {
		l = append(l, Item{Title: fmt.Sprintf("step %d", i+1), Status: StatusDone})
	}
	l[10].Status = StatusActive
	for i := 11; i < 20; i++ {
		l[i].Status = StatusPending
	}
	out := l.RenderWindow("", 5)
	if !strings.Contains(out, "above") || !strings.Contains(out, "below") {
		t.Fatalf("expected collapsed markers, got:\n%s", out)
	}
	if !strings.Contains(out, "step 11") { // the active item must be visible
		t.Fatalf("active item should be in the window:\n%s", out)
	}
	if strings.Count(out, "\n") > 6 { // 5 items + 2 collapse lines max
		t.Fatalf("window too large:\n%s", out)
	}
}

func TestCurrentRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// Empty corpus → empty list, no error.
	if l, err := Current(ctx, st); err != nil || len(l) != 0 {
		t.Fatalf("expected empty, got %v (err %v)", l, err)
	}

	want := FromTitles([]string{"first", "second"})
	if _, err := st.AppendEvent(ctx, store.Event{Kind: EventKind, Actor: "agent", Payload: mustJSON(want.Payload())}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// A newer snapshot supersedes the older one.
	newer := Advance(append(List(nil), want...))
	if _, err := st.AppendEvent(ctx, store.Event{Kind: EventKind, Actor: "agent", Payload: mustJSON(newer.Payload())}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := Current(ctx, st)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if len(got) != 2 || got[0].Status != StatusDone || got[1].Status != StatusActive {
		t.Fatalf("expected latest snapshot, got %+v", got)
	}
}
