// Package sessionprune deletes recorded sessions: one on request (`memcode
// session delete`) and, automatically, every session older than the retention
// window. Both go through Delete, so "a deleted session" means one thing.
//
// A session's local footprint is:
//
//	sessions/<id>/      transcript (messages.json), episodic log (transcript.md,
//	                    events.jsonl), wire trace (wire.jsonl), reports,
//	                    continuations, the live marker
//	checkpoints/<id>/   rewind points
//	state.db events     rows whose payload carries session_id = <id>
//
// Recall has no index of its own: it reads the events table at query time, so
// removing the rows removes the session from recall.
//
// What a session taught memcode is NOT session data and stays: memory.md,
// promoted preferences and lessons (prefs/, lessons/), knowledge, and the event
// kinds that feed them (MemoryKinds). Project configuration (config.json,
// permissions, hooks.json, mcp.json) is never touched.
package sessionprune

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/statedir"
	"github.com/memcode-ai/memcode/internal/store"
)

// DefaultRetentionDays is how long a session is kept after its last activity
// when no setting says otherwise. 0 means keep forever.
const DefaultRetentionDays = 30

// MemoryKinds are the event kinds that feed persistent memory. They carry the
// session id of the session that produced them but outlive it: deleting them
// would quietly un-learn a preference or a lesson.
var MemoryKinds = []string{
	"preference_signal", "lesson_signal", "adherence",
	"decision", "assertion", "user_note",
}

// liveFile marks a session that a running process has open; it holds the pid.
const liveFile = "live.pid"

// stampFile records the last automatic prune of a project.
const stampFile = "session-prune.stamp"

// ErrLive is returned for a session that a running memcode process has open.
var ErrLive = errors.New("session is open in a running memcode process")

// ErrNotFound is returned when no session matches a reference.
var ErrNotFound = errors.New("no such session")

// Info is one recorded session.
type Info struct {
	ID         string
	LastActive time.Time
	Live       bool
}

func sessionsDir(root string) string    { return filepath.Join(statedir.For(root), "sessions") }
func checkpointsDir(root string) string { return filepath.Join(statedir.For(root), "checkpoints") }

// MarkLive records that this process has session id open, and returns the
// function that clears the mark. Best-effort: without a mark the session is
// simply not protected from deletion by another process.
func MarkLive(root, id string) (release func()) {
	if !validID(id) {
		return func() {}
	}
	dir := filepath.Join(sessionsDir(root), id)
	if os.MkdirAll(dir, 0o755) != nil {
		return func() {}
	}
	p := filepath.Join(dir, liveFile)
	pid := os.Getpid()
	if os.WriteFile(p, []byte(strconv.Itoa(pid)+"\n"), 0o644) != nil {
		return func() {}
	}
	return func() {
		// Only clear our own mark: a resume in another process may have rewritten it.
		if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(pid) {
			_ = os.Remove(p)
		}
	}
}

// IsLive reports whether a running process has session id open. A mark left
// by a process that has exited (a crash) does not count.
func IsLive(root, id string) bool {
	if !validID(id) {
		return false
	}
	b, err := os.ReadFile(filepath.Join(sessionsDir(root), id, liveFile))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	return pid == os.Getpid() || processAlive(pid)
}

// validID rejects anything that could step outside the session directories.
func validID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`) && filepath.Base(id) == id
}

// List returns every recorded session of the project, newest first: those
// with files on disk and those known only from the event log. st may be nil.
func List(ctx context.Context, root string, st store.Store) ([]Info, error) {
	last := map[string]time.Time{}
	bump := func(id string, t time.Time) {
		if cur, ok := last[id]; !ok || t.After(cur) {
			last[id] = t
		}
	}
	for _, dir := range []string{sessionsDir(root), checkpointsDir(root)} {
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() || !validID(e.Name()) {
				continue
			}
			bump(e.Name(), newestMod(filepath.Join(dir, e.Name())))
		}
	}
	if st != nil {
		act, err := st.SessionActivity(ctx, MemoryKinds)
		if err != nil {
			return nil, err
		}
		for id, t := range act {
			if validID(id) {
				bump(id, t)
			}
		}
	}
	out := make([]Info, 0, len(last))
	for id, t := range last {
		out = append(out, Info{ID: id, LastActive: t, Live: IsLive(root, id)})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastActive.Equal(out[j].LastActive) {
			return out[i].LastActive.After(out[j].LastActive)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// newestMod is the latest modification time of anything under dir (the
// directory itself included) — the session's last activity on disk.
func newestMod(dir string) time.Time {
	var t time.Time
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Name() == liveFile {
			return nil // opening a session is not activity
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().After(t) {
			t = fi.ModTime()
		}
		return nil
	})
	return t
}

// Resolve maps a reference to exactly one session id: an exact id, or a
// unique prefix ("sess_" optional). An ambiguous prefix is an error naming
// the candidates.
func Resolve(sessions []Info, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("%w: empty session id", ErrNotFound)
	}
	var matches []string
	for _, s := range sessions {
		if s.ID == ref {
			return s.ID, nil
		}
		if strings.HasPrefix(s.ID, ref) || strings.HasPrefix(s.ID, "sess_"+ref) {
			matches = append(matches, s.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("%w matches %q", ErrNotFound, ref)
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("%q is ambiguous: %s", ref, strings.Join(matches, ", "))
	}
}

// Result summarizes what a Delete or Prune removed.
type Result struct {
	Sessions []string // session ids deleted
	Paths    []string // files and directories removed
	Events   int64    // event rows removed
}

// Delete removes every local artifact of the given sessions. A session open
// in a running process is refused with ErrLive (nothing of it is removed);
// one that is already gone is not an error, so deleting is idempotent.
func Delete(ctx context.Context, root string, st store.Store, ids ...string) (Result, error) {
	var res Result
	var live []string
	var del []string
	for _, id := range ids {
		if !validID(id) {
			return res, fmt.Errorf("invalid session id %q", id)
		}
		if IsLive(root, id) {
			live = append(live, id)
			continue
		}
		del = append(del, id)
	}
	for _, id := range del {
		for _, p := range []string{filepath.Join(sessionsDir(root), id), filepath.Join(checkpointsDir(root), id)} {
			if _, err := os.Lstat(p); err != nil {
				continue
			}
			if err := os.RemoveAll(p); err != nil {
				return res, fmt.Errorf("removing %s: %w", p, err)
			}
			res.Paths = append(res.Paths, p)
		}
	}
	if st != nil && len(del) > 0 {
		n, err := st.DeleteSessionEvents(ctx, del, MemoryKinds)
		res.Events = n
		if err != nil {
			return res, err
		}
	}
	res.Sessions = del
	if len(live) > 0 {
		return res, fmt.Errorf("%w: %s", ErrLive, strings.Join(live, ", "))
	}
	return res, nil
}

// Options configure a prune.
type Options struct {
	RetentionDays int       // sessions idle longer than this are deleted; <= 0 keeps everything
	Keep          []string  // session ids never pruned (the current or resumed one)
	Now           time.Time // zero = time.Now()
}

// Prune deletes every session whose last activity is older than the retention
// window, except the kept and live ones, and clears diagnostics, stray wire
// traces and observed-tool logs older than the same window.
func Prune(ctx context.Context, root string, st store.Store, o Options) (Result, error) {
	if o.RetentionDays <= 0 {
		return Result{}, nil
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-time.Duration(o.RetentionDays) * 24 * time.Hour)
	keep := map[string]bool{}
	for _, id := range o.Keep {
		keep[id] = true
	}
	sessions, err := List(ctx, root, st)
	if err != nil {
		return Result{}, err
	}
	var old []string
	for _, s := range sessions {
		if keep[s.ID] || s.Live || !s.LastActive.Before(cutoff) {
			continue
		}
		old = append(old, s.ID)
	}
	res, err := Delete(ctx, root, st, old...)
	if err != nil && !errors.Is(err, ErrLive) {
		return res, err
	}
	// A kept session's own wire trace still ages out: it is a debugging aid,
	// not part of the conversation.
	if entries, err := os.ReadDir(sessionsDir(root)); err == nil {
		for _, e := range entries {
			if e.IsDir() && !IsLive(root, e.Name()) {
				res.Paths = append(res.Paths, removeOlder(filepath.Join(sessionsDir(root), e.Name(), "wire.jsonl"), cutoff)...)
			}
		}
	}
	base := statedir.For(root)
	res.Paths = append(res.Paths, removeOlder(filepath.Join(base, "diagnostics"), cutoff)...)
	res.Paths = append(res.Paths, removeOlder(filepath.Join(base, "observe"), cutoff)...)
	return res, nil
}

// removeOlder removes path if it is a file older than cutoff, or, for a
// directory, every file under it older than cutoff (then any directory left
// empty below it). It returns what it removed.
func removeOlder(path string, cutoff time.Time) []string {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	if !fi.IsDir() {
		if fi.ModTime().Before(cutoff) && os.Remove(path) == nil {
			return []string{path}
		}
		return nil
	}
	var removed, dirs []string
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != path {
				dirs = append(dirs, p)
			}
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) && os.Remove(p) == nil {
			removed = append(removed, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- { // deepest first
		_ = os.Remove(dirs[i]) // fails, harmlessly, unless empty
	}
	return removed
}

// MaybePrune runs Prune at most once a day per project: a stamp file in the
// project's state directory remembers the last run. It reports whether a
// prune ran. Cheap when it does not: one stat.
func MaybePrune(ctx context.Context, root string, st store.Store, o Options) (Result, bool, error) {
	if o.RetentionDays <= 0 {
		return Result{}, false, nil
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	stamp := filepath.Join(statedir.For(root), stampFile)
	if fi, err := os.Stat(stamp); err == nil && now.Sub(fi.ModTime()) < 24*time.Hour && !fi.ModTime().After(now) {
		return Result{}, false, nil
	}
	// Stamp first: a prune that fails part-way is retried tomorrow, not on
	// every launch.
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err == nil {
		if os.WriteFile(stamp, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o644) == nil {
			_ = os.Chtimes(stamp, now, now)
		}
	}
	o.Now = now
	res, err := Prune(ctx, root, st, o)
	return res, true, err
}
