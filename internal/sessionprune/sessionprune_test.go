package sessionprune

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/statedir"
	"github.com/memcode-ai/memcode/internal/store"
)

type fixture struct {
	t    *testing.T
	root string
	base string
	st   store.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Setenv(statedir.EnvHome, t.TempDir())
	root := t.TempDir()
	base := statedir.For(root)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &fixture{t: t, root: root, base: base, st: st}
}

func (f *fixture) write(rel, body string, mod time.Time) string {
	f.t.Helper()
	p := filepath.Join(f.base, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(p, mod, mod); err != nil {
			f.t.Fatal(err)
		}
	}
	return p
}

// session lays down a full session footprint, all of it last touched at mod.
func (f *fixture) session(id string, mod time.Time) {
	f.t.Helper()
	for _, rel := range []string{
		"sessions/" + id + "/messages.json",
		"sessions/" + id + "/transcript.md",
		"sessions/" + id + "/events.jsonl",
		"sessions/" + id + "/wire.jsonl",
		"sessions/" + id + "/reports/r1.md",
		"checkpoints/" + id + "/0001.json",
	} {
		f.write(rel, "x", mod)
	}
	for _, d := range []string{"sessions/" + id + "/reports", "sessions/" + id, "checkpoints/" + id} {
		_ = os.Chtimes(filepath.Join(f.base, d), mod, mod)
	}
	for _, kind := range []string{"agent_session_started", "tool_called", "frustration", "preference_signal", "lesson_signal"} {
		f.event(kind, id, mod)
	}
}

func (f *fixture) event(kind, sid string, ts time.Time) {
	f.t.Helper()
	p, _ := json.Marshal(map[string]any{"session_id": sid, "text": "t"})
	if _, err := f.st.AppendEvent(context.Background(), store.Event{TS: ts, Kind: kind, Payload: p}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(f.base, rel))
	return err == nil
}

// eventKinds returns the kinds of the events stamped with sid.
func (f *fixture) eventKinds(sid string) []string {
	f.t.Helper()
	evs, err := f.st.ListEvents(context.Background(), store.EventFilter{})
	if err != nil {
		f.t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if p["session_id"] == sid {
			kinds = append(kinds, e.Kind)
		}
	}
	return kinds
}

func ids(infos []Info) []string {
	out := make([]string, len(infos))
	for i, s := range infos {
		out[i] = s.ID
	}
	return out
}

// Deleting a session removes its files, checkpoints, wire trace and event rows,
// and nothing else: memory, config, learned signals and other sessions stay.
func TestDeleteRemovesExactlyTheSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()
	f.session("sess_aaaa1111", now)
	f.session("sess_bbbb2222", now)
	keepFiles := []string{"memory.md", "config.json", "permissions", "hooks.json", "mcp.json", "prefs/p1.md", "lessons/l1.md", "knowledge/k.md"}
	for _, rel := range keepFiles {
		f.write(rel, "keep", time.Time{})
	}
	f.event("user_note", "", now) // a project note without a session

	res, err := Delete(ctx, f.root, f.st, "sess_aaaa1111")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Sessions, ",") != "sess_aaaa1111" || res.Events != 3 {
		t.Fatalf("result = %+v, want the one session and its 3 non-memory events", res)
	}
	for _, rel := range []string{"sessions/sess_aaaa1111", "checkpoints/sess_aaaa1111"} {
		if f.exists(rel) {
			t.Errorf("%s survived the delete", rel)
		}
	}
	if got := strings.Join(f.eventKinds("sess_aaaa1111"), ","); got != "preference_signal,lesson_signal" {
		t.Errorf("remaining events of the deleted session = %q, want only the memory kinds", got)
	}
	for _, rel := range append(keepFiles, "sessions/sess_bbbb2222/messages.json", "sessions/sess_bbbb2222/wire.jsonl", "checkpoints/sess_bbbb2222/0001.json") {
		if !f.exists(rel) {
			t.Errorf("%s was removed by deleting another session", rel)
		}
	}
	if n := len(f.eventKinds("sess_bbbb2222")); n != 5 {
		t.Errorf("other session lost events: %d left, want 5", n)
	}
	if n := len(f.eventKinds("")); n != 1 {
		t.Errorf("sessionless note removed")
	}

	// The deleted session no longer lists (its memory rows do not resurrect it),
	// and deleting it again is a no-op.
	list, err := List(ctx, f.root, f.st)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(list), ","); got != "sess_bbbb2222" {
		t.Fatalf("List after delete = %q", got)
	}
	if res, err := Delete(ctx, f.root, f.st, "sess_aaaa1111"); err != nil || res.Events != 0 || len(res.Paths) != 0 {
		t.Fatalf("second delete = %+v, %v; want a no-op", res, err)
	}
}

func TestResolvePrefixes(t *testing.T) {
	infos := []Info{{ID: "sess_ab12"}, {ID: "sess_ab34"}, {ID: "sess_cd56"}}
	for ref, want := range map[string]string{"sess_cd56": "sess_cd56", "cd": "sess_cd56", "sess_ab1": "sess_ab12", "ab3": "sess_ab34"} {
		if got, err := Resolve(infos, ref); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	if _, err := Resolve(infos, "ab"); err == nil || !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), "sess_ab12") || !strings.Contains(err.Error(), "sess_ab34") {
		t.Errorf("ambiguous prefix: err = %v", err)
	}
	if _, err := Resolve(infos, "zz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown ref: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteRejectsPathIDs(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"..", "../x", "a/b", ""} {
		if _, err := Delete(context.Background(), f.root, f.st, id); err == nil {
			t.Errorf("Delete(%q) accepted", id)
		}
	}
}

// A session marked open by a running process is refused; a mark left by a
// process that has exited is not protection.
func TestDeleteRefusesLiveSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.session("sess_live", time.Now())
	release := MarkLive(f.root, "sess_live")
	if _, err := Delete(ctx, f.root, f.st, "sess_live"); !errors.Is(err, ErrLive) {
		t.Fatalf("live delete err = %v, want ErrLive", err)
	}
	if !f.exists("sessions/sess_live/messages.json") || len(f.eventKinds("sess_live")) != 5 {
		t.Fatal("a refused delete removed data")
	}
	release()
	if IsLive(f.root, "sess_live") {
		t.Fatal("released session still live")
	}

	// A dead pid: a process that ran and exited.
	c := exec.Command(os.Args[0], "-test.run=^$")
	if err := c.Run(); err != nil {
		t.Skip("cannot spawn a short-lived process:", err)
	}
	f.write("sessions/sess_live/live.pid", strconv.Itoa(c.Process.Pid), time.Time{})
	if IsLive(f.root, "sess_live") {
		t.Skip("pid reused already") // vanishingly rare
	}
	if _, err := Delete(ctx, f.root, f.st, "sess_live"); err != nil {
		t.Fatalf("stale mark blocked delete: %v", err)
	}
}

func TestPruneRespectsRetentionKeepAndLive(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-40 * 24 * time.Hour)
	f.session("sess_old", old)
	f.session("sess_resumed", old)
	f.session("sess_open", old)
	f.session("sess_recent", now.Add(-2*24*time.Hour))
	f.event("agent_session_started", "sess_dbonly", old) // known only from the event log
	defer MarkLive(f.root, "sess_open")()
	_ = os.Chtimes(filepath.Join(f.base, "sessions/sess_open/live.pid"), old, old)
	f.write("diagnostics/old.json", "x", old)
	f.write("diagnostics/new.json", "x", now)
	f.write("observe/claude-code/old.jsonl", "x", old)
	f.write("memory.md", "keep", old)
	f.write("lessons/l.md", "keep", old)

	res, err := Prune(ctx, f.root, f.st, Options{RetentionDays: 30, Keep: []string{"sess_resumed"}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Sessions, ","); !strings.Contains(got, "sess_old") || !strings.Contains(got, "sess_dbonly") || len(res.Sessions) != 2 {
		t.Fatalf("pruned %q, want sess_old and sess_dbonly", got)
	}
	for _, rel := range []string{"sessions/sess_old", "checkpoints/sess_old", "diagnostics/old.json", "observe/claude-code/old.jsonl",
		"sessions/sess_resumed/wire.jsonl"} { // a kept session's stale wire trace still ages out
		if f.exists(rel) {
			t.Errorf("%s survived the prune", rel)
		}
	}
	for _, rel := range []string{"sessions/sess_resumed/messages.json", "sessions/sess_open/messages.json", "sessions/sess_open/wire.jsonl",
		"sessions/sess_recent/messages.json", "diagnostics/new.json", "memory.md", "lessons/l.md"} {
		if !f.exists(rel) {
			t.Errorf("%s was pruned", rel)
		}
	}
	if len(f.eventKinds("sess_dbonly")) != 0 {
		t.Error("event-only session's rows survived")
	}
}

func TestRetentionZeroKeepsAll(t *testing.T) {
	f := newFixture(t)
	ancient := time.Now().Add(-5 * 365 * 24 * time.Hour)
	f.session("sess_ancient", ancient)
	f.write("diagnostics/old.json", "x", ancient)
	res, ran, err := MaybePrune(context.Background(), f.root, f.st, Options{RetentionDays: 0})
	if err != nil || ran || len(res.Sessions) != 0 {
		t.Fatalf("retention 0: ran=%v res=%+v err=%v", ran, res, err)
	}
	if res, _ := Prune(context.Background(), f.root, f.st, Options{RetentionDays: 0}); len(res.Sessions) != 0 {
		t.Fatal("Prune with retention 0 deleted sessions")
	}
	if !f.exists("sessions/sess_ancient/messages.json") || !f.exists("diagnostics/old.json") {
		t.Fatal("retention 0 removed data")
	}
}

// The automatic prune runs at most once a day per project.
func TestMaybePruneDailyStamp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()
	f.session("sess_first", now.Add(-40*24*time.Hour))
	if _, ran, err := MaybePrune(ctx, f.root, f.st, Options{RetentionDays: 30, Now: now}); err != nil || !ran {
		t.Fatalf("first prune: ran=%v err=%v", ran, err)
	}
	if f.exists("sessions/sess_first") {
		t.Fatal("first prune did not delete the old session")
	}

	f.session("sess_second", now.Add(-40*24*time.Hour))
	if _, ran, _ := MaybePrune(ctx, f.root, f.st, Options{RetentionDays: 30, Now: now.Add(23 * time.Hour)}); ran {
		t.Fatal("pruned twice within a day")
	}
	if !f.exists("sessions/sess_second") {
		t.Fatal("skipped prune deleted a session")
	}
	if _, ran, _ := MaybePrune(ctx, f.root, f.st, Options{RetentionDays: 30, Now: now.Add(25 * time.Hour)}); !ran {
		t.Fatal("did not prune again the next day")
	}
	if f.exists("sessions/sess_second") {
		t.Fatal("next-day prune did not delete the old session")
	}
}
