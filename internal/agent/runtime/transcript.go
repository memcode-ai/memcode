package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Transcript persistence — the byte-for-byte provider message history, saved at
// every turn boundary so a session can be RE-ENTERED (`memcode agent --continue`,
// `/resume`), not just recalled. It complements the episodic log: events.jsonl
// is what happened (memory); messages.json is the live conversation state
// (resumption). Snapshot, not append — compaction rewrites history, so the
// newest snapshot is always the whole truth. Thinking blocks round-trip
// verbatim through wire.Block, which is what the API requires on resume.

const transcriptFile = "messages.json"

type transcript struct {
	SessionID string         `json:"session_id"`
	SavedAt   time.Time      `json:"saved_at"`
	Messages  []wire.Message `json:"messages"`
}

func transcriptPath(root, id string) string {
	return filepath.Join(root, ".memcode", "sessions", id, transcriptFile)
}

// persistTranscript snapshots the chat history for resume (atomic tmp+rename).
// Best-effort: a failed write must never break a turn — the session simply
// won't be resumable.
func (s *Session) persistTranscript(st *ChatState) {
	if s.sessionID == "" || st == nil || len(st.messages) == 0 {
		return
	}
	p := transcriptPath(s.root, s.sessionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(transcript{SessionID: s.sessionID, SavedAt: time.Now().UTC(), Messages: st.messages})
	if err != nil {
		return
	}
	if err := atomicfile.WriteFile(p, b, 0o644); err != nil {
		s.printf("⚠ couldn't save transcript (%v) — this session won't be resumable\n", err)
	}
}

// loadTranscript reads a saved history back.
func loadTranscript(root, id string) ([]wire.Message, error) {
	b, err := os.ReadFile(transcriptPath(root, id))
	if err != nil {
		return nil, err
	}
	var t transcript
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("corrupt transcript for %s: %w", id, err)
	}
	if len(t.Messages) == 0 {
		return nil, fmt.Errorf("session %s has an empty transcript", id)
	}
	return t.Messages, nil
}

// ResolveSession maps a user reference to a resumable session id: ""/"latest"
// → the most recently saved transcript; otherwise an exact id or unique prefix
// ("sess_" optional). Only sessions WITH a saved transcript qualify — older
// sessions predating transcript persistence are recall-only.
func ResolveSession(root, ref string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(root, ".memcode", "sessions"))
	if err != nil {
		return "", fmt.Errorf("no saved sessions under .memcode/sessions")
	}
	type cand struct {
		id  string
		mod time.Time
	}
	var cands []cand
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fi, err := os.Stat(transcriptPath(root, e.Name()))
		if err != nil {
			continue
		}
		cands = append(cands, cand{e.Name(), fi.ModTime()})
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("no resumable sessions yet — transcripts are saved from now on, so the next session will be")
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "latest" {
		return cands[0].id, nil
	}
	var matches []string
	for _, c := range cands {
		if c.id == ref {
			return c.id, nil
		}
		if strings.HasPrefix(c.id, ref) || strings.HasPrefix(c.id, "sess_"+ref) {
			matches = append(matches, c.id)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no resumable session matches %q", ref)
	default:
		return "", fmt.Errorf("%q is ambiguous: %s", ref, strings.Join(matches, ", "))
	}
}

// ResumableSessions lists resumable session ids, newest first (for pickers).
func ResumableSessions(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, ".memcode", "sessions"))
	if err != nil {
		return nil
	}
	type cand struct {
		id  string
		mod time.Time
	}
	var cands []cand
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if fi, err := os.Stat(transcriptPath(root, e.Name())); err == nil {
			cands = append(cands, cand{e.Name(), fi.ModTime()})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.id
	}
	return ids
}

// SetResume asks the NEXT StartChat to re-enter session id with its saved
// history instead of minting a fresh session. Set it from the cmd layer (or a
// slash command) before the front-end starts the chat; StartChat consumes it.
func (s *Session) SetResume(id string) { s.resumeID = id }

// ForkSession copies srcID's saved transcript (and its checkpoints) to a fresh
// session id and returns it — the original session is untouched. Fork-from-disk is
// safe because the transcript persists at every turn boundary and callers refuse to
// fork mid-turn, so disk equals the live history. The episodic log
// (events.jsonl/transcript.md) is DELIBERATELY not copied: session search, recall,
// and the focus reducer scan every session dir, and a duplicated history would
// double-count what happened — the original keeps the episodic truth, and the fork
// starts logging its own from here. Checkpoints ride along (best-effort) so /rewind
// still reaches pre-fork turns in the fork.
func ForkSession(root, srcID string) (string, error) {
	msgs, err := loadTranscript(root, srcID) // validates existence + non-empty
	if err != nil {
		return "", err
	}
	newID := newSessionID()
	p := transcriptPath(root, newID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	b, err := json.Marshal(transcript{SessionID: newID, SavedAt: time.Now().UTC(), Messages: msgs})
	if err != nil {
		return "", err
	}
	if err := atomicfile.WriteFile(p, b, 0o644); err != nil {
		return "", err
	}
	src := filepath.Join(root, ".memcode", "checkpoints", srcID)
	if fi, err := os.Stat(src); err == nil && fi.IsDir() {
		_ = os.CopyFS(filepath.Join(root, ".memcode", "checkpoints", newID), os.DirFS(src))
	}
	return newID, nil
}
