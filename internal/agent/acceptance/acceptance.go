// Package acceptance closes memcode's interaction loop by reading the room AFTER
// the work: did the agent's changes survive contact with the human? Git is the
// objective evidence most agents never look at — a commit is the strongest "yes",
// a revert the strongest "no", a manual edit a "close, but".
//
//	agent acts → user reacts → git reveals accepted/corrected/rejected → memory adjusts
//
// It compares each finished agent session's per-file result hashes against the
// current git state and records a session_outcome event (only when the verdict is
// definitive — an undecided session is re-checked on the next scan). The room
// reducer already consumes those outcomes, and `learn` weighting will next.
package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/edit"
	"github.com/memcode-ai/memcode/internal/agent/gitporcelain"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/store"
)

// Outcome verdicts (string values match what room.Gather reads).
const (
	Accepted  = "accepted"  // committed substantially intact (high confidence)
	Corrected = "corrected" // agent's files manually changed after (medium)
	Rejected  = "rejected"  // patch reverted / reset / discarded (high)
	Unknown   = "unknown"   // undecided — not recorded, re-checked later
)

// Result is one reconciled session.
type Result struct {
	SessionID  string `json:"session_id"`
	Outcome    string `json:"outcome"`
	Confidence string `json:"confidence"` // high | medium
	Evidence   string `json:"evidence"`
}

// fileState is the per-file evidence the classifier reasons over (pure input).
type fileState struct {
	path           string
	postHash       string // the agent's result hash
	curHash        string // "" = file absent now
	dirty          bool   // still has uncommitted changes
	committedSince bool   // a commit after the session baseline touches it
}

// classify turns per-file evidence into a session verdict. Conservative: it only
// returns a definitive outcome when the evidence supports it.
func classify(files []fileState) (outcome, confidence, evidence string) {
	var accepted, corrected, rejected, unknown int
	for _, f := range files {
		switch {
		case f.curHash == "":
			rejected++ // the agent's file is gone
		case f.dirty:
			if f.curHash == f.postHash {
				unknown++ // still pending — unchanged since the agent
			} else {
				corrected++ // human edited further, not yet committed
			}
		case f.committedSince:
			if f.curHash == f.postHash {
				accepted++ // committed intact
			} else {
				corrected++ // committed after a human refinement
			}
		default: // clean, not committed, content differs ⇒ discarded
			if f.curHash != f.postHash {
				rejected++
			} else {
				unknown++
			}
		}
	}
	total := accepted + corrected + rejected + unknown
	switch {
	case total == 0 || unknown == total:
		return Unknown, "", ""
	case rejected > 0 && rejected >= accepted+corrected:
		return Rejected, "high", evidenceStr("reverted/discarded", rejected, total)
	case corrected > 0:
		return Corrected, "medium", evidenceStr("manually changed after the agent", corrected, total)
	case accepted > 0:
		return Accepted, "high", evidenceStr("committed intact", accepted, total)
	default:
		return Unknown, "", ""
	}
}

// session accumulates a session's events during the scan.
type session struct {
	baseline   string            // head_sha at session start
	finished   bool              //
	postHash   map[string]string // path → last result hash
	order      []string          // stable file order
	reconciled bool              // already has a session_outcome
}

// Reconcile scans the event log for finished, not-yet-reconciled agent sessions
// and records a session_outcome for each one whose fate git can now confirm.
// No-op outside a git repo. Returns the outcomes it recorded.
func Reconcile(ctx context.Context, st store.Store, root string) ([]Result, error) {
	if !isGitRepo(ctx, root) {
		return nil, nil
	}
	evs, err := st.ListEvents(ctx, store.EventFilter{Kinds: []string{
		string(events.KindAgentSessionStarted),
		string(events.KindAgentSessionFinished),
		string(events.KindFileEdited),
		string(events.KindSessionOutcome),
	}})
	if err != nil {
		return nil, err
	}

	sessions := map[string]*session{}
	get := func(id string) *session {
		if sessions[id] == nil {
			sessions[id] = &session{postHash: map[string]string{}}
		}
		return sessions[id]
	}
	for _, e := range evs {
		id := strField(e.Payload, "session_id")
		if id == "" {
			continue
		}
		switch events.Kind(e.Kind) {
		case events.KindAgentSessionStarted:
			get(id).baseline = strField(e.Payload, "head_sha")
		case events.KindAgentSessionFinished:
			get(id).finished = true
		case events.KindFileEdited:
			s := get(id)
			p := strField(e.Payload, "path")
			if p == "" {
				continue
			}
			if _, seen := s.postHash[p]; !seen {
				s.order = append(s.order, p)
			}
			s.postHash[p] = strField(e.Payload, "hash")
		case events.KindSessionOutcome:
			get(id).reconciled = true
		}
	}

	dirty := dirtySet(ctx, root)
	var results []Result
	for id, s := range sessions {
		if !s.finished || s.reconciled || len(s.order) == 0 {
			continue
		}
		var files []fileState
		for _, p := range s.order {
			cur, _ := edit.Hash(root, p)
			files = append(files, fileState{
				path:           p,
				postHash:       s.postHash[p],
				curHash:        cur,
				dirty:          dirty[p],
				committedSince: committedSince(ctx, root, s.baseline, p),
			})
		}
		outcome, conf, ev := classify(files)
		if outcome == Unknown {
			continue // leave it open for the next scan
		}
		_, _ = events.Append(ctx, st, events.KindSessionOutcome, "reconciler", map[string]any{
			"session_id": id, "outcome": outcome, "confidence": conf, "evidence": ev,
		})
		results = append(results, Result{SessionID: id, Outcome: outcome, Confidence: conf, Evidence: ev})
	}
	return results, nil
}

// --- git helpers ---

func isGitRepo(ctx context.Context, root string) bool {
	return exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--is-inside-work-tree").Run() == nil
}

func dirtySet(ctx context.Context, root string) map[string]bool {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	set := map[string]bool{}
	if err != nil {
		return set
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		if i := strings.Index(p, " -> "); i >= 0 {
			p = p[i+4:]
		}
		// core.quotePath=true C-quotes non-ASCII paths ("caf\303\251.txt").
		set[gitporcelain.Unquote(p)] = true
	}
	return set
}

// committedSince reports whether any commit after baseline touches path.
func committedSince(ctx context.Context, root, baseline, path string) bool {
	if baseline == "" {
		return false
	}
	out, err := exec.CommandContext(ctx, "git", "-C", root,
		"log", "--oneline", baseline+"..HEAD", "--", path).Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}

func strField(p json.RawMessage, key string) string {
	if len(p) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(p, &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func evidenceStr(what string, n, total int) string {
	return fmt.Sprintf("%d/%d agent file(s) %s", n, total, what)
}
