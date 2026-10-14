// Package observe is memcode's LAYER A for externally-sourced attention
// signals — Claude Code's and Codex's own local session transcripts — plus
// the LAYER-B merge that hands a cross-source turn stream to
// focus.ReduceTurns. It never writes to .memcode/sessions: that stays
// memcode's own, untouched, first-person episodic log. Its own on-disk
// format lives at .memcode/observe/<source>/<source-session-id>.jsonl — a
// bespoke minimal format, deliberately not sessionlog.Writer, so an
// external observation can never be mistaken for a native session on disk.
// Every record here is a CLAIM about what a tool did, not a verified fact
// (see FileClaim, and package acceptance's CorroborateClaim for checking one
// against git/filesystem state).
//
// COST ARCHITECTURE (load-bearing, not incidental — see doctrine for the
// per-claim confidence rule this pairs with):
//
//   - Zero LLM calls anywhere in this package. Ingestion (an importer's
//     Import), normalization (into Record), and this file's merge are all
//     deterministic parsing/bookkeeping — exactly how memcode treats its OWN
//     session log today. There is no proactive summarization pass; if
//     dogfooding later shows real semantic loss (architectural rationale,
//     abandoned approaches that never produced a Turn-worthy event), that is
//     a separate, explicitly-costed addition — not assumed here.
//   - A Record is normalized to exactly what focus.ReduceTurns consumes, not
//     a transcript copy: Text is capped at write time (maxText), well below
//     a raw transcript turn. A 50k-token external session becomes a handful
//     of capped records on disk, not a replica of the transcript.
//   - Load is bounded on BOTH axes — maxFilesPerSource (recent sessions per
//     source) and maxRecordsPerFile — so its parse cost stays flat as total
//     ingested history grows to hundreds of sessions, mirroring
//     sessionlog's own burst-window philosophy (see focus.BurstSessions).
//   - What finally reaches a chat system prompt is bounded a second,
//     independent time by focus.ReduceTurns' existing per-bucket/per-strand
//     caps (maxPerBucket, maxFullAsk) — so the digest stays small REGARDLESS
//     of how much external history exists, exactly as it already does for
//     memcode's own history. Raw transcript text never reaches that far.
package observe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/focus"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/sessionlog"
)

// maxText bounds a Record's Text at WRITE time — belt-and-suspenders with
// focus's own maxFullAsk (which caps a strand's Full at REDUCE time): an
// importer must never persist a raw multi-KB transcript chunk even before
// reduction gets a chance to cap it. Generous enough to carry a real ask
// whole, far short of a transcript turn.
const maxText = 2000

// Clip truncates s to maxText runes (rune-safe), for an importer to apply
// before ever writing a Record's Text to disk.
func Clip(s string) string {
	r := []rune(s)
	if len(r) <= maxText {
		return s
	}
	return string(r[:maxText]) + "…"
}

// FileClaim is an unverified claim, from a transcript, that a file was
// written/edited to a specific resulting content. Mirrors focus.FileClaim —
// kept as its own type here (the on-disk JSON shape) rather than embedding
// focus.FileClaim directly, so this package's wire format doesn't silently
// change if focus's in-memory type grows fields later.
type FileClaim struct {
	Path        string    `json:"path"`
	ClaimedHash string    `json:"claimed_hash"`
	ClaimedAt   time.Time `json:"claimed_at,omitempty"`
}

// Record is one externally-observed turn, normalized to exactly what
// focus.ReduceTurns consumes — never a transcript copy. Source and
// SourceSessionID are NOT persisted: the directory name IS the source, the
// filename IS the session id (same "the dir/file name is the identity, don't
// duplicate it" rule sessionlog.Record.SessionID already follows).
type Record struct {
	TS        time.Time  `json:"ts"`
	Kind      string     `json:"kind"` // one of sessionlog.Kind* — the shared vocabulary
	Text      string     `json:"text,omitempty"`
	Tool      string     `json:"tool,omitempty"`
	Input     string     `json:"input,omitempty"`
	FileClaim *FileClaim `json:"file_claim,omitempty"`
}

// toTurn converts a Record read back off disk into the shared focus.Turn
// representation, stamping the provenance the directory/file structure
// carried (never persisted redundantly in the record itself).
func (r Record) toTurn(source, sourceSessionID string) focus.Turn {
	t := focus.Turn{
		TS:              r.TS,
		Kind:            r.Kind,
		Text:            r.Text,
		Tool:            r.Tool,
		Input:           r.Input,
		Source:          source,
		SourceSessionID: sourceSessionID,
	}
	if r.FileClaim != nil {
		t.FileClaim = &focus.FileClaim{Path: r.FileClaim.Path, ClaimedHash: r.FileClaim.ClaimedHash, ClaimedAt: r.FileClaim.ClaimedAt}
	}
	return t
}

// Writer appends normalized Records for one external session to
// .memcode/observe/<source>/<sourceSessionID>.jsonl. A nil Writer is safe to
// Append/Close on, matching sessionlog.Writer's degrade-gracefully contract.
type Writer struct {
	mu sync.Mutex
	f  *os.File
}

func observeDir(root string) string {
	return filepath.Join(root, config.DirName, "observe")
}

// Open creates (or reopens, append-only) the on-disk log for one external
// source's session.
func Open(root, source, sourceSessionID string) (*Writer, error) {
	dir := filepath.Join(observeDir(root), source)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, sourceSessionID+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f}, nil
}

// Append writes one Record, clipping Text defensively even if the caller
// forgot to (see Clip). Safe on a nil Writer; safe under concurrent calls.
func (w *Writer) Append(r Record) error {
	if w == nil {
		return nil
	}
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	r.Text = Clip(r.Text)
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.f.Write(append(b, '\n'))
	return err
}

// Close flushes and closes the underlying file. Safe on a nil Writer.
func (w *Writer) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// maxFilesPerSource and maxRecordsPerFile bound Load's parse cost — recent
// sessions per source, recent records per session — so re-reading on every
// consolidated-view call stays flat regardless of how many sessions a source
// has ever contributed. Deliberately the same shape as sessionlog's burst
// window (focus.BurstSessions / BurstPerSession): "where you left off"
// always includes the latest, without cost growing with total history.
const (
	maxFilesPerSource = 5
	maxRecordsPerFile = 200
)

// Load merges memcode's own current work-burst turns (read via sessionlog,
// converted, NEVER written here) with every external source's recent
// observations under .memcode/observe/, sorted into one chronological
// stream — the actual cross-source "consolidated memory" ReduceTurns
// reduces. excludeSessionID is the live session, if any (mirrors
// focus.FromLog). Best-effort: a read failure on one file is logged and
// skipped, never fatal to the merge.
func Load(ctx context.Context, root, excludeSessionID string) []focus.Turn {
	var turns []focus.Turn

	recs, err := sessionlog.RecentBurstExcluding(root, excludeSessionID, focus.BurstSessions, focus.BurstGap, focus.BurstPerSession)
	if err != nil {
		log.Printf("observe: native session burst read failed: %v", err)
	}
	if excludeSessionID != "" {
		if cur, err := sessionlog.Recent(root, excludeSessionID, 0); err != nil {
			log.Printf("observe: native session read failed for %s: %v", excludeSessionID, err)
		} else if len(cur) > 0 {
			recs = append(recs, cur...)
		}
	}
	turns = append(turns, focus.TurnsFromRecords(recs)...)

	base := observeDir(root)
	sourceDirs, err := os.ReadDir(base)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("observe: read %s failed: %v", base, err)
		}
		return sortTurns(turns)
	}
	for _, sd := range sourceDirs {
		if ctx.Err() != nil {
			break
		}
		if !sd.IsDir() {
			continue
		}
		source := sd.Name()
		for _, fp := range recentFiles(filepath.Join(base, source), maxFilesPerSource) {
			sessID := strings.TrimSuffix(filepath.Base(fp), ".jsonl")
			recs, err := readRecords(fp)
			if err != nil {
				log.Printf("observe: read %s failed: %v", fp, err)
				continue
			}
			for _, r := range recs {
				turns = append(turns, r.toTurn(source, sessID))
			}
		}
	}
	return sortTurns(turns)
}

// Adapter is an external source's deterministic transcript reader —
// implemented by internal/importers/claudecode and internal/importers/codex.
// Discover finds candidate transcript files for the given repo roots (see
// gitlog.Roots); Import parses ONE file starting at a byte offset
// (incremental) up to maxBytes new bytes, returning the Records found and
// the offset to resume from next time. No LLM call anywhere in an Adapter —
// deterministic parsing only, matching how memcode treats its own session
// log.
type Adapter interface {
	Source() string
	Discover(repoRoots []string) []string
	Import(path string, fromOffset, maxBytes int64) (recs []Record, newOffset int64, err error)
}

// maxBytesPerSync bounds the TOTAL new bytes read+parsed across every file
// in ONE Sync call — the hard limit that keeps ingestion cost flat
// regardless of backlog size (a vendor's rollout files have been observed up
// to ~2GB in the wild). The rest of a large backlog is picked up on a LATER
// call via the watermark, never blocked on in one pass.
const maxBytesPerSync = 2 << 20 // 2 MiB

// watermarks maps an absolute transcript file path to the next byte offset
// to resume reading from — so re-syncing a source never re-reads, re-parses,
// or re-sends content it has already processed.
type watermarks map[string]int64

func watermarkPath(root, source string) string {
	return filepath.Join(observeDir(root), source, ".watermark.json")
}

func loadWatermarks(root, source string) watermarks {
	b, err := os.ReadFile(watermarkPath(root, source))
	if err != nil {
		return watermarks{}
	}
	var w watermarks
	if json.Unmarshal(b, &w) != nil {
		return watermarks{}
	}
	return w
}

func saveWatermarks(root, source string, w watermarks) error {
	dir := filepath.Join(observeDir(root), source)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return os.WriteFile(watermarkPath(root, source), b, 0o644)
}

// Sync does ONE bounded, deterministic ingestion pass for an adapter: for
// each file it discovers, reads only the bytes appended since that file's
// watermark (never the whole transcript again), redacts every Record's Text
// through redact BEFORE it ever touches disk, and appends to that session's
// observe log. Best-effort across files — one file's error is recorded but
// doesn't stop the rest; the watermark only advances for files that
// actually succeeded, so a failure is retried next time rather than silently
// skipped forever.
func Sync(ctx context.Context, root string, repoRoots []string, a Adapter, redact func(string) string) error {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	source := a.Source()
	wm := loadWatermarks(root, source)
	budget := int64(maxBytesPerSync)
	var firstErr error
	for _, path := range a.Discover(repoRoots) {
		if ctx.Err() != nil || budget <= 0 {
			break
		}
		from := wm[path]
		recs, newOffset, err := a.Import(path, from, budget)
		if err != nil {
			log.Printf("observe: %s import %s failed: %v", source, path, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if consumed := newOffset - from; consumed > 0 {
			budget -= consumed
		}
		if newOffset == from {
			continue // nothing new in this file
		}
		if len(recs) > 0 {
			sessID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			w, err := Open(root, source, sessID)
			if err != nil {
				log.Printf("observe: %s open %s failed: %v", source, sessID, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			for _, r := range recs {
				r.Text = redact(r.Text)
				if err := w.Append(r); err != nil {
					log.Printf("observe: %s append %s failed: %v", source, sessID, err)
				}
			}
			w.Close()
		}
		wm[path] = newOffset // only advance past what we actually wrote
	}
	if err := saveWatermarks(root, source, wm); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// ReadNewLines reads complete lines appended to path since fromOffset, up to
// maxBytes of NEW content, and returns the lines plus the offset to resume
// from. A trailing partial line (either genuinely still being written, or
// simply cut off by maxBytes) is left unconsumed — newOffset never advances
// past it — so it is re-read whole next time: incremental reading of an
// append-only JSONL log must never split a line across two syncs. Shared by
// every Adapter so this bookkeeping isn't duplicated per source.
func ReadNewLines(path string, fromOffset, maxBytes int64) (lines [][]byte, newOffset int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fromOffset, err
	}
	defer f.Close()
	if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
		return nil, fromOffset, err
	}
	r := bufio.NewReader(io.LimitReader(f, maxBytes))
	offset := fromOffset
	for {
		line, rerr := r.ReadString('\n')
		if rerr != nil {
			break // partial trailing line or EOF — stop, don't advance past it
		}
		offset += int64(len(line))
		lines = append(lines, []byte(strings.TrimRight(line, "\n")))
	}
	return lines, offset, nil
}

// HasAny reports whether anything has ever been written under
// .memcode/observe/ — a cheap directory-presence check callers use to skip
// Load entirely (and keep paying zero cost) when no external source has
// ever been ingested, which is the common case today.
func HasAny(root string) bool {
	entries, err := os.ReadDir(observeDir(root))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		inner, err := os.ReadDir(filepath.Join(observeDir(root), e.Name()))
		if err == nil && len(inner) > 0 {
			return true
		}
	}
	return false
}

// recentFiles returns up to n *.jsonl files in dir, most-recently-modified
// first.
func recentFiles(dir string, n int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type ref struct {
		path string
		mod  time.Time
	}
	var refs []ref
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		refs = append(refs, ref{path: filepath.Join(dir, e.Name()), mod: info.ModTime()})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].mod.After(refs[j].mod) })
	if len(refs) > n {
		refs = refs[:n]
	}
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.path
	}
	return out
}

// readRecords scans a Record JSONL file defensively: a malformed line is
// skipped, never fatal — external transcripts/observations are an
// unversioned, best-effort format. Keeps only the most recent
// maxRecordsPerFile.
func readRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue // malformed line: skip, never abort
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	if len(out) > maxRecordsPerFile {
		out = out[len(out)-maxRecordsPerFile:]
	}
	return out, nil
}

// sortTurns returns turns in chronological order (oldest first) — the order
// ReduceTurns expects a stream in, same as sessionlog's readers.
func sortTurns(turns []focus.Turn) []focus.Turn {
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].TS.Before(turns[j].TS) })
	return turns
}
