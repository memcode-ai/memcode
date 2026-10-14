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
	"time"

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
		switch classifyFile(f) {
		case Accepted:
			accepted++
		case Corrected:
			corrected++
		case Rejected:
			rejected++
		default:
			unknown++
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

// classifyFile turns ONE file's evidence into its outcome — accepted
// (committed intact), corrected (changed again after the baseline, by a
// human or another tool), rejected (reverted/discarded/absent), or unknown
// (still pending, no verdict yet). The single source of truth both
// classify's multi-file session aggregation and CorroborateClaim's
// single-claim check build on — one deterministic rule, not two.
func classifyFile(f fileState) string {
	switch {
	case f.curHash == "":
		return Rejected // the claimed file is gone
	case f.dirty:
		if f.curHash == f.postHash {
			return Unknown // still pending — unchanged since the claim
		}
		return Corrected // edited further (by anyone), not yet committed
	case f.committedSince:
		if f.curHash == f.postHash {
			return Accepted // committed intact
		}
		return Corrected // committed after a further refinement
	default: // clean, not committed, content differs ⇒ discarded
		if f.curHash != f.postHash {
			return Rejected
		}
		return Unknown
	}
}

// outcomeConfidence maps a single-file outcome to the SAME confidence tier
// classify's multi-file aggregation uses for its dominant outcome — one
// evidence-to-confidence rule, used by both call paths.
func outcomeConfidence(outcome string) string {
	switch outcome {
	case Accepted, Rejected:
		return "high"
	case Corrected:
		return "medium"
	default:
		return ""
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

// CorroborateClaim checks ANY claimed file edit — not just one of memcode's
// own agent sessions — against CURRENT git/filesystem state: committed
// intact, changed since, reverted/absent, or still-dirty (matching or
// differing). It is the same deterministic evidence Reconcile already uses
// for memcode's own session outcomes (classifyFile), made callable for a
// claim from ANY source — an external tool's transcript, a focus.FileClaim.
// This is a NEW, separate call path: Reconcile's own event-log-driven loop,
// and what it records, are completely untouched.
//
// The claim and this corroboration are two different kinds of evidence
// about two different questions (what a tool SAYS it did vs. what survived
// to disk) — never collapse them into one number; show both.
func CorroborateClaim(ctx context.Context, root, path, claimedHash string, since time.Time) (verdict, confidence, evidence string) {
	if !isGitRepo(ctx, root) {
		return Unknown, "", "not a git repository"
	}
	cur, _ := edit.Hash(root, path)
	f := fileState{
		path:           path,
		postHash:       claimedHash,
		curHash:        cur,
		dirty:          dirtySet(ctx, root)[path],
		committedSince: committedSinceTime(ctx, root, path, since),
	}
	outcome := classifyFile(f)
	return outcome, outcomeConfidence(outcome), claimEvidence(outcome, f)
}

// claimEvidence renders CorroborateClaim's outcome as a short, specific
// prose fact — the single-claim counterpart to evidenceStr's multi-file
// session phrasing.
func claimEvidence(outcome string, f fileState) string {
	switch outcome {
	case Accepted:
		return "committed intact"
	case Corrected:
		if f.dirty {
			return "changed since the claim, not yet committed"
		}
		return "committed after a further change"
	case Rejected:
		if f.curHash == "" {
			return "file no longer exists"
		}
		return "reverted/discarded — current content differs, no commit since"
	default:
		return "still pending — no commit or further change since the claim yet"
	}
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

// committedSinceTime reports whether any commit at or after since touches
// path — the time-based counterpart to committedSince's SHA-based baseline,
// for a claim that carries a timestamp (e.g. focus.FileClaim.ClaimedAt)
// rather than one of memcode's own sessions' head_sha.
func committedSinceTime(ctx context.Context, root, path string, since time.Time) bool {
	if since.IsZero() {
		return false
	}
	out, err := exec.CommandContext(ctx, "git", "-C", root,
		"log", "--oneline", "--since", since.UTC().Format(time.RFC3339), "--", path).Output()
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
