// Package sessionlog is memcode's episodic memory: a local, append-only record of
// the high-level causal trail of a session — user messages, assistant messages,
// meaningful actions (commands, edits, commits), and approvals — written to disk
// as it happens, independent of the LLM context window. It is NOT a stdout
// landfill: internal reads/greps and raw tool output stay out (raw mechanics
// belong in an optional debug trace, not canonical memory).
//
// The doctrine it serves:
//
//	working context → fast but lossy
//	session log     → episodic truth (why/when/under whose approval/side effects)
//	git             → code-diff truth
//	claims/memories → distilled doctrine
//
// events.jsonl is the append-only source of truth (replayable); transcript.md is
// a human-readable rendering written alongside. Readers are deterministic scans —
// no model, no index — so the agent can self-recall before acting on fuzzy memory.
package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"github.com/memcode-ai/memcode/internal/config"
)

// Record kinds. Keep these stable — they're the on-disk contract.
const (
	KindSessionStarted   = "session_started"
	KindUserMessage      = "user_message"
	KindAssistantMessage = "assistant_message"
	KindToolCall         = "tool_call"      // a meaningful action: command, edit, commit — NOT internal reads
	KindApproval         = "approval"       // a user allow/deny decision
	KindToolResult       = "tool_result"    // reserved for an optional debug trace; not written to the canonical log
	KindCompaction       = "compaction"     // older turns summarized in-session (the warm layer's durable record)
	KindPlanPresented    = "plan_presented" // a plan was presented for approval — full text + slug (recoverable via RecentPlans)
	KindPlanCancelled    = "plan_cancelled" // planning was ABANDONED before execution — the ask is still open work
	KindSessionFinished  = "session_finished"
	KindPreferenceSignal = "preference_signal" // a captured user directive — the CANONICAL copy for the prefs reducer
	KindLessonSignal     = "lesson_signal"     // a distilled lesson — the CANONICAL copy for the lessons reducer
	KindContextInlined   = "context_inlined"   // which promoted rules (lesson/pref ids) rode this session's system prompt
	KindAdherence        = "adherence"         // a rule-adherence verdict about TargetSession — the CANONICAL copy for reducer weighting
	KindFacts            = "facts"             // one atomic extracted fact about THIS session (post-session cognition) — indexed by ranked search
)

// Record is one append-only line in events.jsonl. Fields are sparse (omitempty);
// which ones are set depends on Kind.
type Record struct {
	TS        time.Time `json:"ts"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text,omitempty"`        // user/assistant message text
	Tool      string    `json:"tool,omitempty"`        // tool_call/result tool name
	Input     string    `json:"input,omitempty"`       // tool_call input (redacted JSON)
	Decision  string    `json:"decision,omitempty"`    // approval: approved | denied | …
	ToolUseID string    `json:"tool_use_id,omitempty"` // links a result to its call (debug trace)
	Content   string    `json:"content,omitempty"`     // tool_result output (debug trace) / $ shell output
	IsError   bool      `json:"is_error,omitempty"`    // tool_result / $ shell failed
	Exit      int       `json:"exit,omitempty"`        // $ shell command exit code
	Model     string    `json:"model,omitempty"`       // session_started
	Mode      string    `json:"mode,omitempty"`        // session_started
	HeadSHA   string    `json:"head_sha,omitempty"`    // session_started
	Slug      string    `json:"slug,omitempty"`        // plan_presented: the saved-plan slug (~/.memcode/plans/<slug>.md)
	Axis      string    `json:"axis,omitempty"`        // preference_signal: which axis (workflow/gating/…)
	Scope     string    `json:"scope,omitempty"`       // preference_signal: this-repo vs global
	Strength  float64   `json:"strength,omitempty"`    // preference_signal / lesson_signal: evidence weight
	Trigger   string    `json:"trigger,omitempty"`     // lesson_signal: the recurring failure condition
	Strategy  string    `json:"strategy,omitempty"`    // lesson_signal: what to do when the trigger holds
	Entities  []string  `json:"entities,omitempty"`    // facts: lowercase entity keys the fact is about (feeds the session entity graph)

	// Post-session learning loop fields.
	LessonIDs     []string `json:"lesson_ids,omitempty"`     // context_inlined: promoted lesson ids in this session's prompt
	PrefIDs       []string `json:"pref_ids,omitempty"`       // context_inlined: confirmed pref ids in this session's prompt
	RuleKind      string   `json:"rule_kind,omitempty"`      // adherence: "lesson" | "pref"
	RuleID        string   `json:"rule_id,omitempty"`        // adherence: the rule's stable id
	Verdict       string   `json:"verdict,omitempty"`        // adherence: followed | violated | not_applicable
	Outcome       string   `json:"outcome,omitempty"`        // adherence: the target session's git outcome (accepted/corrected/rejected)
	TargetSession string   `json:"target_session,omitempty"` // adherence / lesson_signal: the session the verdict/lesson is ABOUT (the record lives in the judging session's log)

	// SessionID is stamped IN MEMORY by the multi-session readers (RecentBurstExcluding,
	// Recent) so a merged record knows which session it came from — used to name a
	// thread's source session in orientation. json:"-": never written to disk (the dir
	// name IS the id; persisting it would be redundant and could drift).
	SessionID string `json:"-"`
}

// Writer appends records for one session to .memcode/sessions/<id>/.
type Writer struct {
	mu         sync.Mutex
	id         string
	events     *os.File
	transcript *os.File
}

// Open creates (or reopens) the on-disk log for a session and returns a Writer.
// A nil Writer is safe to Append/Close on, so callers can ignore the error and
// degrade gracefully if the directory can't be created.
func Open(root, sessionID string) (*Writer, error) {
	dir := filepath.Join(sessionsDir(root), sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ev, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	tr, err := os.OpenFile(filepath.Join(dir, "transcript.md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		ev.Close()
		return nil, err
	}
	return &Writer{id: sessionID, events: ev, transcript: tr}, nil
}

// Append writes one record to the append-only log and mirrors a human line into
// the transcript. Safe on a nil Writer and safe under concurrent calls.
func (w *Writer) Append(r Record) {
	if w == nil {
		return
	}
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// events.jsonl is the CANONICAL, replayable log — a dropped line is silent data loss
	// (and desyncs it from the transcript). Surface marshal/write failures instead of
	// swallowing them (best-effort: Append is fire-and-forget with no error return).
	b, err := json.Marshal(r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memcode: sessionlog marshal failed (event NOT recorded): %v\n", err)
		return // don't write a transcript line for a record that never hit the canonical log
	}
	if _, err := w.events.Write(append(b, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "memcode: sessionlog write failed (event may be lost): %v\n", err)
	}
	if line := transcriptLine(r); line != "" {
		w.transcript.WriteString(line)
	}
}

// Close flushes and closes the underlying files. Safe on a nil Writer.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	if w.transcript != nil {
		err = w.transcript.Close()
	}
	if w.events != nil {
		if e := w.events.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

func sessionsDir(root string) string {
	return filepath.Join(root, config.DirName, "sessions")
}

// --- readers (deterministic; the agent's self-recall primitive) ---

// Recent returns the last n records of one session (n<=0 = all).
func Recent(root, sessionID string, n int) ([]Record, error) {
	recs, err := readRecords(filepath.Join(sessionsDir(root), sessionID, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	if n > 0 && len(recs) > n {
		recs = recs[len(recs)-n:]
	}
	stampSession(recs, sessionID)
	return recs, nil
}

// stampSession sets the in-memory SessionID on each record (see Record.SessionID).
func stampSession(recs []Record, id string) {
	for i := range recs {
		recs[i].SessionID = id
	}
}

// LatestRecent returns the last n records of the most recently active session
// (by directory mtime) — the signal orientation/prediction pulls from to know
// "where you left off." Empty (nil, nil) if no sessions exist yet.
func LatestRecent(root string, n int) ([]Record, error) {
	return LatestRecentExcluding(root, "", n)
}

// LatestRecentExcluding returns the last n records of the most recently active
// session whose id differs from excludeID — i.e. "the previous distinct session"
// when excludeID is the current one. This is what answers "what was the last
// session about?" from a brand-new session, which is already non-empty (it holds
// the user's opening message) and would otherwise shadow the prior one. Empty
// (nil, nil) if there is no other session.
func LatestRecentExcluding(root, excludeID string, n int) ([]Record, error) {
	refs, err := sessionRefs(root)
	if err != nil || len(refs) == 0 {
		return nil, err
	}
	for _, ref := range refs { // newest first
		if ref.id == excludeID {
			continue
		}
		recs, err := readRecords(ref.path)
		if err != nil {
			return nil, err
		}
		if n > 0 && len(recs) > n {
			recs = recs[len(recs)-n:]
		}
		return recs, nil
	}
	return nil, nil
}

// RecentBurstExcluding returns the merged records of the current WORK BURST of
// prior sessions (excludeID skipped), in CHRONOLOGICAL order — oldest session
// first, so a reducer sees one continuous stream and later work supersedes
// earlier work naturally. perSession caps each session's tail (0 = all).
//
// The burst rule replaces both bad windows. A flat "last session" baked in
// amnesia (the thread from two sessions ago — minutes older — vanished); a
// flat "last K sessions" hauls year-old work into today's orientation just to
// fill a quota. Instead: ALWAYS take the most recent prior session, however
// old — that is "where you left off" — then keep walking older only while the
// gap between CONSECUTIVE sessions stays within burstGap, capped at
// maxSessions. Sessions clustered in one stretch of work arrive together; a
// long break ends the window.
func RecentBurstExcluding(root, excludeID string, maxSessions int, burstGap time.Duration, perSession int) ([]Record, error) {
	refs, err := sessionRefs(root)
	if err != nil || len(refs) == 0 {
		return nil, err
	}
	var picked []sessionRef // newest first
	for _, ref := range refs {
		if ref.id == excludeID {
			continue
		}
		if len(picked) > 0 && burstGap > 0 && picked[len(picked)-1].mod.Sub(ref.mod) > burstGap {
			break // a long break in the work — older sessions are a different arc
		}
		picked = append(picked, ref)
		if maxSessions > 0 && len(picked) == maxSessions {
			break
		}
	}
	var out []Record
	for i := len(picked) - 1; i >= 0; i-- { // reverse → oldest session first
		recs, err := readRecords(picked[i].path)
		if err != nil {
			continue // one unreadable session must not blind the whole window
		}
		if perSession > 0 && len(recs) > perSession {
			recs = recs[len(recs)-perSession:]
		}
		stampSession(recs, picked[i].id)
		out = append(out, recs...)
	}
	return out, nil
}

// Sidequests returns the user messages of one session — the sequence of things
// the user actually asked for, in order.
func Sidequests(root, sessionID string) ([]Record, error) {
	recs, err := Recent(root, sessionID, 0)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, r := range recs {
		if r.Kind == KindUserMessage {
			out = append(out, r)
		}
	}
	return out, nil
}

// SessionSummary is a one-line digest of a past session for "what were the last
// couple sessions about?" — the headline is the session's first user request.
type SessionSummary struct {
	ID       string
	Started  time.Time
	Headline string
	Actions  int // meaningful tool calls (commands/edits) — a sense of how much happened
}

// RecentSessions returns the most recent sessions newest-first, EXCLUDING excludeID
// (the current one). n<=0 means all. This is the "recent sessions" affordance the
// agent needs to answer about session history — distinct from Recent(), which reads
// a SINGLE session's records.
func RecentSessions(root, excludeID string, n int) ([]SessionSummary, error) {
	refs, err := sessionRefs(root)
	if err != nil {
		return nil, err
	}
	var out []SessionSummary
	for _, ref := range refs {
		if ref.id == excludeID {
			continue
		}
		recs, err := readRecords(ref.path)
		if err != nil || len(recs) == 0 {
			continue
		}
		sum := SessionSummary{ID: ref.id}
		for _, r := range recs {
			switch r.Kind {
			case KindSessionStarted:
				if sum.Started.IsZero() {
					sum.Started = r.TS
				}
			case KindUserMessage:
				if sum.Headline == "" {
					sum.Headline = firstLine(r.Text, 90)
				}
			case KindToolCall:
				sum.Actions++
			}
		}
		if sum.Started.IsZero() {
			sum.Started = recs[0].TS
		}
		if sum.Headline == "" {
			sum.Headline = "(no request recorded)"
		}
		out = append(out, sum)
		if n > 0 && len(out) >= n {
			break
		}
	}
	return out, nil
}

// Search scans every session (newest first) for records whose text/content/input
// /tool contains query (case-insensitive). An empty query matches everything.
// Returns at most n hits (n<=0 = unlimited).
func Search(root, query string, n int) ([]Record, error) {
	q := strings.TrimSpace(query)
	paths, err := allSessionFiles(root)
	if err != nil {
		return nil, err
	}

	// Empty query: the old "everything, newest first" behavior.
	if q == "" {
		var hits []Record
		for _, p := range paths {
			recs, _ := readRecords(p)
			id := filepath.Base(filepath.Dir(p))
			for i := len(recs) - 1; i >= 0; i-- {
				r := recs[i]
				r.SessionID = id
				hits = append(hits, r)
				if n > 0 && len(hits) >= n {
					return hits, nil
				}
			}
		}
		return hits, nil
	}

	// Ranked search (see rank.go). Hits carry SessionID so callers can say
	// where a result came from — the old scan never attributed hits.
	bySession := map[string][]Record{}
	for _, p := range paths {
		recs, _ := readRecords(p)
		if len(recs) == 0 {
			continue
		}
		id := filepath.Base(filepath.Dir(p))
		stampSession(recs, id)
		bySession[id] = recs
	}
	hits := rankRecords(bySession, q, time.Now())
	if n > 0 && len(hits) > n {
		hits = hits[:n]
	}
	return hits, nil
}

// LastShell returns the most recent `$` direct-shell command (Tool "shell"),
// across all sessions newest-first — its command (Input), output (Content), and
// exit (Exit/IsError). This is the data behind explain/fix-last: "why did that
// fail?" / "fix it" consult it. Empty (false) when no `$` command has run.
func LastShell(root string) (Record, bool) {
	paths, err := allSessionFiles(root)
	if err != nil {
		return Record{}, false
	}
	for _, p := range paths {
		recs, _ := readRecords(p)
		for i := len(recs) - 1; i >= 0; i-- {
			if recs[i].Kind == KindToolCall && recs[i].Tool == "shell" {
				return recs[i], true
			}
		}
	}
	return Record{}, false
}

// Commits returns the git commit/push tool calls across all sessions (newest
// first) — the accountability trail of "did we already commit/push this?".
func Commits(root string) ([]Record, error) {
	paths, err := allSessionFiles(root)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, p := range paths {
		recs, _ := readRecords(p)
		for i := len(recs) - 1; i >= 0; i-- {
			r := recs[i]
			if r.Kind == KindToolCall && IsGitCommitPush(r.Input) {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// IsGitCommitPush reports whether a tool-call input is an actual `git commit`/`git push`
// INVOCATION — parsed with the shell AST, never a substring match. `rg "git commit"` (the
// phrase inside a quoted search arg) is NOT a commit; the old substring check flagged it,
// marking a strand Completed and logging a phantom commit. The input may be a raw command
// or a bash tool's JSON ({"command":"…"}); both are handled.
func IsGitCommitPush(input string) bool {
	cmd := input
	if t := strings.TrimSpace(input); strings.HasPrefix(t, "{") {
		var j struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(t), &j) == nil && j.Command != "" {
			cmd = j.Command
		}
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return false
	}
	found := false
	syntax.Walk(f, func(n syntax.Node) bool {
		ce, ok := n.(*syntax.CallExpr)
		if ok && len(ce.Args) >= 2 && litWord(ce.Args[0]) == "git" {
			if sub := litWord(ce.Args[1]); sub == "commit" || sub == "push" {
				found = true
				return false
			}
		}
		return !found
	})
	return found
}

// litWord returns a word's literal text when it's a single unquoted/quoted literal
// (the command-head and subcommand cases we classify); "" for anything with an
// expansion or substitution (which we deliberately don't treat as a plain literal).
func litWord(w *syntax.Word) string {
	if w == nil || len(w.Parts) != 1 {
		return ""
	}
	switch p := w.Parts[0].(type) {
	case *syntax.Lit:
		return p.Value
	case *syntax.SglQuoted:
		return p.Value
	case *syntax.DblQuoted:
		if len(p.Parts) == 1 {
			if l, ok := p.Parts[0].(*syntax.Lit); ok {
				return l.Value
			}
		}
	}
	return ""
}

// Digest is a compact, reduced projection of a session's raw events — the form
// orientation/prediction should consume (NOT raw events.jsonl, which is noisy
// and private). It answers "where did we leave off": what was asked, what was
// committed/pushed, how much was tested/edited.
type Digest struct {
	Requests []string // user asks in order — the session's sidequests
	Commits  []string // git commit/push commands run
	Tests    int      // verification runs (test/vet/build/lint)
	Edits    int      // file-edit tool calls
}

// Thread is one strand of work in a session: a user request and the meaningful
// actions taken under it. A session's Threads read like a checklist of "what we
// did," each with a line of detail — the right grain for recall (signal, not the
// raw event firehose; drill into events.jsonl when you need 100%).
type Thread struct {
	Request string
	Actions []string
}

// Recap groups records into Threads: each user message starts a strand, and the
// commands/edits/approvals that follow attach to it. This is the "we built these
// N things, with a bit about each" view.
func Recap(recs []Record) []Thread {
	var threads []Thread
	cur := -1
	add := func(a string) {
		if a == "" {
			return
		}
		if cur < 0 {
			threads = append(threads, Thread{Request: "(session)"})
			cur = 0
		}
		threads[cur].Actions = append(threads[cur].Actions, a)
	}
	for _, r := range recs {
		switch r.Kind {
		case KindUserMessage:
			threads = append(threads, Thread{Request: firstLine(r.Text, 160)})
			cur = len(threads) - 1
		case KindToolCall:
			add(actionLine(r))
		case KindApproval:
			add(r.Decision + ": " + firstLine(r.Text, 80))
		}
	}
	return threads
}

// HasActions reports whether any thread carries a recorded action (command, edit,
// approval) — i.e. the session actually did something rather than just exchanging
// messages. A brand-new session that has only said "hi" has none; that's the cue
// to recap the previous distinct session instead of this near-empty one.
func HasActions(threads []Thread) bool {
	for _, t := range threads {
		if len(t.Actions) > 0 {
			return true
		}
	}
	return false
}

// RenderRecap formats threads as a numbered checklist with a drill-deeper hint.
func RenderRecap(threads []Thread) string {
	if len(threads) == 0 {
		return "nothing recorded in this session yet."
	}
	var b strings.Builder
	n := 0
	for _, t := range threads {
		if t.Request == "" {
			continue
		}
		n++
		fmt.Fprintf(&b, "%d. %s\n", n, t.Request)
		for _, a := range t.Actions {
			fmt.Fprintf(&b, "   - %s\n", a)
		}
	}
	b.WriteString("\n(dig deeper: memcode session recent | search <term>)")
	return strings.TrimRight(b.String(), "\n")
}

// actionLine summarizes a tool call as a short, human verb phrase.
func actionLine(r Record) string {
	switch r.Tool {
	case "bash":
		return "ran " + firstLine(jsonField(r.Input, "command"), 100)
	case "edit_file":
		return "edited " + jsonField(r.Input, "path")
	case "explore":
		return "explored: " + firstLine(jsonField(r.Input, "question"), 80)
	case "todo":
		return "updated the task checklist"
	case "web_search":
		return "searched the web: " + firstLine(jsonField(r.Input, "query"), 80)
	case "ask_user":
		return "asked: " + firstLine(jsonField(r.Input, "question"), 80)
	case "":
		return ""
	default:
		return r.Tool + " " + firstLine(r.Input, 80)
	}
}

// jsonField pulls one string field out of a tool-call input blob; falls back to
// the raw (clipped) input if it isn't the expected shape.
func jsonField(input, field string) string {
	var m map[string]any
	if json.Unmarshal([]byte(input), &m) == nil {
		if v, ok := m[field].(string); ok {
			return v
		}
	}
	return firstLine(input, 80)
}

// Reduce projects raw records into a Digest. This is the "session reducer" seam:
// raw log → compact facts → orientation. Keep it deterministic and lossy.
func Reduce(recs []Record) Digest {
	var d Digest
	for _, r := range recs {
		switch r.Kind {
		case KindUserMessage:
			if t := firstLine(r.Text, 140); t != "" {
				d.Requests = append(d.Requests, t)
			}
		case KindToolCall:
			low := strings.ToLower(r.Input)
			switch {
			case IsGitCommitPush(r.Input):
				// AST-parsed, never substring: `rg "git commit"` (the phrase in a search arg)
				// is NOT a commit. The old substring check logged phantom commits. See
				// [[never-regex-parse-shell]].
				d.Commits = append(d.Commits, firstLine(r.Input, 140))
			case strings.Contains(low, "go test"), strings.Contains(low, "go vet"),
				strings.Contains(low, "npm test"), strings.Contains(low, "pytest"),
				strings.Contains(low, "go build"):
				d.Tests++
			}
			if r.Tool == "edit_file" {
				d.Edits++
			}
		}
	}
	return d
}

// Lines renders the digest as compact one-liners for an orientation prompt.
func (d Digest) Lines() []string {
	var out []string
	reqs := d.Requests
	if len(reqs) > 8 { // keep the most recent asks
		reqs = reqs[len(reqs)-8:]
	}
	for _, q := range reqs {
		out = append(out, "asked: "+q)
	}
	for _, c := range d.Commits {
		out = append(out, "ran: "+c)
	}
	if d.Tests > 0 {
		out = append(out, fmt.Sprintf("ran tests/checks ×%d", d.Tests))
	}
	if d.Edits > 0 {
		out = append(out, fmt.Sprintf("edited files ×%d", d.Edits))
	}
	return out
}

func readRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r Record
		if json.Unmarshal(line, &r) == nil {
			recs = append(recs, r)
		}
	}
	return recs, sc.Err()
}

// sessionRef is one session on disk: its id (directory name) and events path.
type sessionRef struct {
	id   string
	path string
	mod  time.Time // last-ACTIVITY time (events.jsonl mtime) — the recency signal for burst windowing
}

// sessionRefs lists every session (id + events.jsonl path), newest session first by
// last ACTIVITY. Recency is the events.jsonl file's mtime, which updates on every append
// — NOT the directory's mtime, which POSIX only bumps on entry create/delete, so a
// long-lived session worked over days looked "old" (created early) next to a short later
// one. Falls back to the dir mtime when the log file isn't there yet. Empty when none.
func sessionRefs(root string) ([]sessionRef, error) {
	entries, err := os.ReadDir(sessionsDir(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var refs []sessionRef
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mod := time.Time{}
		if info, err := e.Info(); err == nil {
			mod = info.ModTime() // dir mtime — fallback
		}
		path := filepath.Join(sessionsDir(root), e.Name(), "events.jsonl")
		if fi, err := os.Stat(path); err == nil {
			mod = fi.ModTime() // last append = real activity time
		}
		refs = append(refs, sessionRef{e.Name(), path, mod})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].mod.After(refs[j].mod) })
	return refs, nil
}

// PreferenceSignals returns every preference_signal record across ALL sessions on disk,
// oldest-first, each stamped with its source session id. This is the CANONICAL read for
// the prefs reducer's backfill: the SQLite events table is a derived index, so after a
// state.db wipe the signals are recovered from the append-only files here.
func PreferenceSignals(root string) ([]Record, error) {
	refs, err := sessionRefs(root)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, ref := range refs { // sessionRefs is newest-first; collect then sort oldest-first
		recs, _ := readRecords(ref.path)
		for _, r := range recs {
			if r.Kind == KindPreferenceSignal {
				r.SessionID = ref.id
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}

// LessonSignals returns every lesson_signal record across all sessions, oldest
// first, each stamped with its session id. The lessons reducer's backfill path —
// same contract as PreferenceSignals (files canonical, SQLite derived).
func LessonSignals(root string) ([]Record, error) {
	refs, err := sessionRefs(root)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, ref := range refs {
		recs, _ := readRecords(ref.path)
		for _, r := range recs {
			if r.Kind == KindLessonSignal {
				r.SessionID = ref.id
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}

// AdherenceRecords returns every adherence record across all sessions, oldest
// first — the reducers' backfill path for adherence weighting (files canonical,
// SQLite derived; same contract as LessonSignals).
func AdherenceRecords(root string) ([]Record, error) {
	refs, err := sessionRefs(root)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, ref := range refs {
		recs, _ := readRecords(ref.path)
		for _, r := range recs {
			if r.Kind == KindAdherence {
				r.SessionID = ref.id
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}

// SessionRecords returns the full record list of ONE session by id, oldest first
// — the post-session learning loop reads a finished session's trail to build the
// adherence digest. (nil, nil) when the session has no log.
func SessionRecords(root, id string) ([]Record, error) {
	path := filepath.Join(sessionsDir(root), id, "events.jsonl")
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	recs, err := readRecords(path)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		recs[i].SessionID = id
	}
	return recs, nil
}

// PlanRec is a presented plan recovered from a session log — the full markdown plus its slug and
// where/when it was presented. The canonical, project-scoped record of a plan (independent of the
// user-level ~/.memcode/plans store), so a plan is recoverable even if that store is empty.
type PlanRec struct {
	Slug    string
	Title   string
	Text    string
	Session string
	TS      time.Time
}

// RecentPlans scans every session's log for presented-plan records, newest first, deduped by slug
// (the most recent revision of each plan wins). limit ≤ 0 means no limit. Records with no slug
// (legacy) are kept individually. This is the recovery path recall reaches for when the user-level
// store doesn't have a plan — e.g. a plan from a prior session, a wiped store, or a new machine.
func RecentPlans(root string, limit int) ([]PlanRec, error) {
	refs, err := sessionRefs(root)
	if err != nil {
		return nil, err
	}
	bySlug := map[string]PlanRec{}
	var anon []PlanRec
	for _, ref := range refs {
		recs, err := readRecords(ref.path)
		if err != nil {
			continue
		}
		for _, r := range recs {
			if r.Kind != KindPlanPresented || strings.TrimSpace(r.Text) == "" {
				continue
			}
			pr := PlanRec{Slug: r.Slug, Title: planTitle(r.Text), Text: r.Text, Session: ref.id, TS: r.TS}
			if r.Slug == "" {
				anon = append(anon, pr)
				continue
			}
			// Latest revision of a slug wins. Sessions are walked newest-first and records in file
			// (append) order, so on a timestamp tie the later-read record is the newer revision —
			// use !Before (>=) so it replaces, rather than After which would keep the older one.
			if prev, ok := bySlug[r.Slug]; !ok || !pr.TS.Before(prev.TS) {
				bySlug[r.Slug] = pr
			}
		}
	}
	out := make([]PlanRec, 0, len(bySlug)+len(anon))
	for _, pr := range bySlug {
		out = append(out, pr)
	}
	out = append(out, anon...)
	sort.Slice(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// planTitle reads a plan's title from its first non-empty line, stripping leading markdown "#"s
// and a "Plan:" label, clipped to one line.
func planTitle(text string) string {
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		ln = strings.TrimSpace(strings.TrimLeft(ln, "#"))
		for _, p := range []string{"PLAN:", "Plan:", "plan:"} {
			ln = strings.TrimSpace(strings.TrimPrefix(ln, p))
		}
		return firstLine(ln, 80)
	}
	return ""
}

// allSessionFiles lists every session's events.jsonl, newest session first.
func allSessionFiles(root string) ([]string, error) {
	refs, err := sessionRefs(root)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(refs))
	for i, r := range refs {
		paths[i] = r.path
	}
	return paths, nil
}

// transcriptLine renders one record as a human-readable transcript line.
func transcriptLine(r Record) string {
	ts := r.TS.Local().Format("15:04:05")
	switch r.Kind {
	case KindSessionStarted:
		return fmt.Sprintf("# session — %s · model %s · %s mode · HEAD %s\n\n", r.TS.Local().Format("Jan 2 15:04"), r.Model, r.Mode, short(r.HeadSHA))
	case KindUserMessage:
		return fmt.Sprintf("## › you · %s\n%s\n\n", ts, r.Text)
	case KindAssistantMessage:
		return fmt.Sprintf("**memcode** · %s\n%s\n\n", ts, r.Text)
	case KindToolCall:
		return fmt.Sprintf("- ⏺ %s %s\n", r.Tool, firstLine(r.Input, 100))
	case KindApproval:
		return fmt.Sprintf("- ✓ %s: %s\n", r.Decision, firstLine(r.Text, 100))
	case KindToolResult:
		mark := "⎿"
		if r.IsError {
			mark = "⚠"
		}
		return fmt.Sprintf("  %s %s\n", mark, firstLine(r.Content, 120))
	case KindPlanPresented:
		return fmt.Sprintf("\n_◆ plan presented %s — %s_\n%s\n\n", ts, r.Slug, r.Text)
	case KindPlanCancelled:
		return fmt.Sprintf("\n_◆ plan CANCELLED %s — %s (still open work)_\n\n", ts, firstLine(r.Text, 120))
	case KindCompaction:
		return fmt.Sprintf("\n_⊙ context compacted %s — %s_\n\n", ts, firstLine(r.Text, 120))
	case KindSessionFinished:
		return fmt.Sprintf("\n_— session ended %s_\n\n", ts)
	}
	return ""
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}
