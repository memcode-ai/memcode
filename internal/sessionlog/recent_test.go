package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RecentSessions enumerates prior sessions (newest first) with a headline, and
// EXCLUDES the current one — the affordance for "what were the last couple sessions
// about?" that was missing (the agent had no way to list multiple prior sessions).
func TestRecentSessionsEnumeratesPriorsExcludingCurrent(t *testing.T) {
	root := t.TempDir()

	write := func(id, ask string) {
		w, err := Open(root, id)
		if err != nil {
			t.Fatal(err)
		}
		w.Append(Record{Kind: KindUserMessage, Text: ask})
		w.Close()
	}
	write("sess_a", "fix the parser")
	write("sess_b", "add a flag")
	write("sess_c", "current work in progress") // the active one

	got, err := RecentSessions(root, "sess_c", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 prior sessions (current excluded), got %d: %+v", len(got), got)
	}
	heads := got[0].Headline + " | " + got[1].Headline
	if !strings.Contains(heads, "fix the parser") || !strings.Contains(heads, "add a flag") {
		t.Errorf("headlines should be each session's first request: %q", heads)
	}
	for _, s := range got {
		if s.ID == "sess_c" {
			t.Error("current session must be excluded")
		}
	}
}

// RecentBurstExcluding is the orientation window: the current work burst of
// prior sessions merged into ONE chronological stream (oldest session first),
// current session excluded. A single-session lookback dropped any thread from
// two sessions ago — amnesia in a product named memcode.
func TestRecentBurstExcludingMergesChronologically(t *testing.T) {
	root := t.TempDir()
	write := func(id string, asks ...string) {
		w, err := Open(root, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range asks {
			w.Append(Record{Kind: KindUserMessage, Text: a})
		}
		w.Close()
	}
	write("sess_old", "update to gpt 5.6")
	write("sess_mid", "first ask", "second ask", "third ask")
	write("sess_new", "fix the themes alias")
	write("sess_cur", "current work")
	// Pin the recency order via events.jsonl mtimes (sessionRefs sorts by last activity).
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"sess_old", "sess_mid", "sess_new", "sess_cur"} {
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(sessionsDir(root), id, "events.jsonl"), ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	recs, err := RecentBurstExcluding(root, "sess_cur", 5, 48*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	var asks []string
	for _, r := range recs {
		if r.Kind == KindUserMessage {
			asks = append(asks, r.Text)
		}
	}
	if len(asks) != 5 {
		t.Fatalf("want 5 user asks across priors (current excluded), got %v", asks)
	}
	if asks[0] != "update to gpt 5.6" || asks[len(asks)-1] != "fix the themes alias" {
		t.Fatalf("must be chronological, oldest session first: %v", asks)
	}

	// sessions cap keeps only the newest K priors.
	recs, err = RecentBurstExcluding(root, "sess_cur", 1, 48*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 || recs[len(recs)-1].Text != "fix the themes alias" {
		t.Fatalf("sessions=1 must return only the newest prior: %+v", recs)
	}
	for _, r := range recs {
		if r.Text == "update to gpt 5.6" {
			t.Fatal("sessions=1 must not include older sessions")
		}
	}

	// perSession caps each session's tail.
	recs, err = RecentBurstExcluding(root, "sess_cur", 5, 48*time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Text == "first ask" || r.Text == "second ask" {
			t.Fatalf("perSession=1 must keep only each session's tail: %+v", recs)
		}
	}
}

// The burst boundary: the most recent prior session ALWAYS loads (even days
// old — "where you left off"), but a long break ends the window, so year-old
// sessions never ride along just to fill the session cap.
func TestRecentBurstStopsAtLongGap(t *testing.T) {
	root := t.TempDir()
	write := func(id, ask string) {
		w, err := Open(root, id)
		if err != nil {
			t.Fatal(err)
		}
		w.Append(Record{Kind: KindUserMessage, Text: ask})
		w.Close()
	}
	// Age the events.jsonl FILE (recency = last activity, the file's mtime), not the dir.
	touch := func(id string, ts time.Time) {
		if err := os.Chtimes(filepath.Join(sessionsDir(root), id, "events.jsonl"), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	write("sess_yr1", "ancient thing one")
	write("sess_yr2", "ancient thing two")
	write("sess_burst1", "gpt 5.6 migration")
	write("sess_burst2", "fix themes alias")
	write("sess_cur", "current")
	touch("sess_yr1", now.Add(-370*24*time.Hour))
	touch("sess_yr2", now.Add(-365*24*time.Hour))
	touch("sess_burst1", now.Add(-3*time.Hour))
	touch("sess_burst2", now.Add(-1*time.Hour))
	touch("sess_cur", now)

	recs, err := RecentBurstExcluding(root, "sess_cur", 5, 48*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	var asks []string
	for _, r := range recs {
		if r.Kind == KindUserMessage {
			asks = append(asks, r.Text)
		}
	}
	if len(asks) != 2 || asks[0] != "gpt 5.6 migration" || asks[1] != "fix themes alias" {
		t.Fatalf("burst must be today's cluster only, oldest first: %v", asks)
	}

	// A 2-day break before the ONLY prior session: it still loads (where you
	// left off is where you left off, however long ago).
	solo := t.TempDir()
	w, err := Open(solo, "sess_old")
	if err != nil {
		t.Fatal(err)
	}
	w.Append(Record{Kind: KindUserMessage, Text: "resume the migration"})
	w.Close()
	old := time.Now().Add(-2 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(sessionsDir(solo), "sess_old", "events.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	recs, err = RecentBurstExcluding(solo, "sess_now", 5, 48*time.Hour, 0)
	if err != nil || len(recs) == 0 {
		t.Fatalf("the most recent prior session must ALWAYS load: %v %v", recs, err)
	}
}
