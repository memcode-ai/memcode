// Package checkpoint stores per-turn PRE-IMAGES of files the agent edits, so a
// bad run can be rewound without touching git: before edit_file mutates a
// file, its current bytes (or its absence) are snapshotted under
// .memcode/checkpoints/<session>/<seq>/. Rewinding to turn N restores every
// touched file to its earliest pre-image from turns N..latest — the state the
// tree had before turn N ran. Shadow copies only: git stays untouched, and
// changes made OUTSIDE the agent's edit tool (bash, the user's editor) are not
// captured — this is the agent-edit undo, not a filesystem time machine.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

const keepCheckpoints = 50 // per session; oldest pruned on Begin

// Log is one session's checkpoint store.
type Log struct {
	root    string // repo root (snapshot paths are relative to it)
	dir     string // .memcode/checkpoints/<session>
	nextSeq int
}

// Manifest describes one checkpoint (one turn that edited files).
type Manifest struct {
	Seq   int       `json:"seq"`
	Label string    `json:"label"` // the user turn that caused the edits, trimmed
	At    time.Time `json:"at"`
	Files []File    `json:"files"`
}

// File is one snapshotted pre-image.
type File struct {
	Path    string `json:"path"`    // repo-relative
	Existed bool   `json:"existed"` // false → the edit CREATED it (rewind deletes it)
}

// Checkpoint is an open checkpoint accumulating snapshots for the current turn.
type Checkpoint struct {
	log      *Log
	manifest Manifest
	saved    map[string]bool
}

// New opens (or creates) the checkpoint log for a session.
func New(root, sessionID string) *Log {
	l := &Log{root: root, dir: filepath.Join(root, ".memcode", "checkpoints", sessionID)}
	l.nextSeq = 1
	for _, m := range l.list() {
		if m.Seq >= l.nextSeq {
			l.nextSeq = m.Seq + 1
		}
	}
	return l
}

// Begin opens a checkpoint for one turn. Nothing is written until the first
// Snapshot, so turns that never edit cost nothing. Prunes old checkpoints.
func (l *Log) Begin(label string) *Checkpoint {
	if l == nil {
		return nil
	}
	label = strings.TrimSpace(label)
	if len(label) > 120 {
		label = label[:120] + "…"
	}
	l.prune()
	cp := &Checkpoint{log: l, saved: map[string]bool{}}
	cp.manifest = Manifest{Seq: l.nextSeq, Label: label, At: time.Now().UTC()}
	l.nextSeq++
	return cp
}

// Snapshot records path's current state (bytes or absence) once per
// checkpoint. relPath may be absolute (it is normalized against the root).
// Best-effort: snapshot failures never block the edit itself.
func (cp *Checkpoint) Snapshot(relPath string) {
	if cp == nil || relPath == "" {
		return
	}
	if abs, err := filepath.Abs(relPath); err == nil {
		if r, err := filepath.Rel(cp.log.root, abs); err == nil && !strings.HasPrefix(r, "..") {
			relPath = r
		}
	}
	if cp.saved[relPath] {
		return
	}
	cp.saved[relPath] = true

	src := filepath.Join(cp.log.root, relPath)
	b, err := os.ReadFile(src)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return // unreadable (perms?) — don't record a lie
	}
	dir := cp.dir()
	dst := filepath.Join(dir, "files", relPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	if existed {
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return
		}
	}
	cp.manifest.Files = append(cp.manifest.Files, File{Path: relPath, Existed: existed})
	mb, err := json.MarshalIndent(cp.manifest, "", "  ")
	if err != nil {
		return
	}
	// Atomic: a crash mid-write must not leave a truncated manifest, which List rejects —
	// that would silently drop this turn's rewind point.
	_ = atomicfile.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644)
}

// Empty reports whether the checkpoint captured nothing (no edits this turn).
func (cp *Checkpoint) Empty() bool { return cp == nil || len(cp.manifest.Files) == 0 }

func (cp *Checkpoint) dir() string {
	return filepath.Join(cp.log.dir, strconv.Itoa(cp.manifest.Seq))
}

// List returns this session's checkpoints that captured edits, oldest first.
func (l *Log) List() []Manifest {
	if l == nil {
		return nil
	}
	return l.list()
}

func (l *Log) list() []Manifest {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil
	}
	var out []Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(l.dir, e.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var m Manifest
		if json.Unmarshal(b, &m) == nil && len(m.Files) > 0 {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Restore rewinds the tree to BEFORE checkpoint seq: every file touched from
// seq onward is restored to its earliest pre-image (created files are
// deleted), and the consumed checkpoints are dropped. Returns the restored
// file list.
func (l *Log) Restore(seq int) ([]string, error) {
	if l == nil {
		return nil, fmt.Errorf("no checkpoints for this session")
	}
	ms := l.list()
	// Earliest pre-image wins: walk ASC from seq; first sighting of a path is
	// its state before this whole span ran.
	type preimage struct {
		cpSeq   int
		existed bool
	}
	first := map[string]preimage{}
	var consumed []int
	for _, m := range ms {
		if m.Seq < seq {
			continue
		}
		consumed = append(consumed, m.Seq)
		for _, f := range m.Files {
			if _, seen := first[f.Path]; !seen {
				first[f.Path] = preimage{cpSeq: m.Seq, existed: f.Existed}
			}
		}
	}
	if len(first) == 0 {
		return nil, fmt.Errorf("no checkpoint at or after %d", seq)
	}
	var restored []string
	var errs []string
	for path, pi := range first {
		dst := filepath.Join(l.root, path)
		if !pi.existed {
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Sprintf("%s: %v", path, err))
				continue
			}
			restored = append(restored, path+" (deleted — the edit had created it)")
			continue
		}
		b, err := os.ReadFile(filepath.Join(l.dir, strconv.Itoa(pi.cpSeq), "files", path))
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		restored = append(restored, path)
	}
	sort.Strings(restored)
	if len(errs) > 0 {
		// Do NOT drop the consumed checkpoints: some files failed to restore, and these
		// pre-images are the only copy — keeping them lets the user retry the rewind. (A
		// successful restore below consumes them.)
		return restored, fmt.Errorf("some files could not be restored (pre-images kept for retry): %s", strings.Join(errs, "; "))
	}
	for _, c := range consumed {
		_ = os.RemoveAll(filepath.Join(l.dir, strconv.Itoa(c)))
	}
	return restored, nil
}

// prune keeps the newest keepCheckpoints checkpoint dirs.
func (l *Log) prune() {
	ms := l.list()
	for len(ms) >= keepCheckpoints {
		_ = os.RemoveAll(filepath.Join(l.dir, strconv.Itoa(ms[0].Seq)))
		ms = ms[1:]
	}
}
