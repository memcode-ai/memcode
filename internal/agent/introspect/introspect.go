// Package introspect is memcode's read-only intelligence surface — the commands
// the agent reaches through the single `memcode` tool (overview, map, context,
// why, recall, next, recap, memories, sources, session, acceptance, doctor, jobs)
// and the TUI's "orient me" slash shortcuts, plus the two ambiguity classifiers
// (plan-intent, follow-up steer-vs-queue) and the personality greeting.
//
// It was carved off the agent Session god-object. The Engine reaches Session
// state through a narrow Deps struct (data fields + a few callbacks), so the
// package depends on neither runtime nor the full Session type — the import
// direction stays runtime → introspect (a clean leaf below the agent hub).
package introspect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/acceptance"
	"github.com/memcode-ai/memcode/internal/agent/focus"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/assemble"
	"github.com/memcode-ai/memcode/internal/doctor"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/gitlog"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/objectives"
	"github.com/memcode-ai/memcode/internal/overview"
	"github.com/memcode-ai/memcode/internal/predict"
	"github.com/memcode-ai/memcode/internal/provenance"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/recall"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/sources"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
	"github.com/memcode-ai/memcode/internal/textutil"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Redactor is the redaction seam (runtime's *secrets.Redactor satisfies it).
// Defined here so introspect needn't import the secrets package.
type Redactor interface {
	Redact(s string) string
}

// Deps is the subset of *runtime.Session the introspection commands need. The
// runtime.Session fills it (via s.introspect()) and constructs an Engine. Data
// fields hold session state; the callbacks are the runtime-internal seams the
// commands call through (tool-line rendering, chat-spec request building, and the
// job-management dispatch that stays on Session).
type Deps struct {
	Root         string
	Store        store.Store
	Runner       *llm.Runner
	Prov         provider.ModelProvider // raw provider, for capability checks (doctor) + the ClassifyFollowup guard
	Redactor     Redactor
	SessionID    string
	Model        string
	Personality  string // chosen voice; drives PersonalityBlurb
	LastUserText string // the active task (set by scoreTurn); ClassifyFollowup's anchor

	// ToolLine prints a tool-call marker line (runtime's s.toolLine). May be nil.
	ToolLine func(shown bool, verb, arg, status string, failed bool)
	// Complete runs a side-channel LLM call through the session's instrumented plumbing
	// (wire trace — runtime's s.sideComplete). May be nil (tests) → falls back to Runner.
	Complete func(ctx context.Context, purpose llm.Purpose, req wire.Request) (wire.Response, error)
	// ExtraChecks appends session-scoped rows to /doctor (runtime's classifier traffic —
	// s.classifierChecks). May be nil.
	ExtraChecks func() []doctor.Result
	// ChatRequest stamps the session chat spec (personality fact + redaction) onto
	// a request — runtime's s.chatSpec("").request. Used by PersonalityBlurb. May be nil.
	ChatRequest func(wire.Request) wire.Request
	// Jobs dispatches the shell-management command (list|kill|tail) — runtime's
	// s.introspectJobs, which stays on Session (it's the shell domain, not introspection).
	Jobs func(target string) (string, bool)
}

// Engine answers the read-only intelligence commands over a Deps. Construct one
// per call with New; it holds no mutable state of its own.
type Engine struct {
	d Deps
}

// New builds an Engine over the given dependencies.
func New(d Deps) *Engine { return &Engine{d: d} }

func (e *Engine) redact(s string) string { return e.d.Redactor.Redact(s) }

// complete routes through the session's instrumented side-channel plumbing when wired
// (wire trace + failure visibility), falling back to the bare Runner otherwise.
func (e *Engine) complete(ctx context.Context, purpose llm.Purpose, req wire.Request) (wire.Response, error) {
	if e.d.Complete != nil {
		return e.d.Complete(ctx, purpose, req)
	}
	return e.d.Runner.Complete(ctx, purpose, req)
}

// MemcodeTool dispatches the single `memcode` agent tool into memcode's own
// intelligence — calling the internal packages DIRECTLY (no subprocess, no shell
// strings), so the agent uses memcode through one structured, instrumentable tool
// instead of re-deriving everything by hand.
func (e *Engine) MemcodeTool(ctx context.Context, input json.RawMessage) (string, bool) {
	var in tools.MemcodeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return err.Error(), true
	}
	cmd := strings.ToLower(strings.TrimSpace(in.Command))
	// The Memcode introspection tool (overview/session/recall/context) is the agent reading
	// its OWN memory to orient — internal plumbing, not work the user needs to watch. It ran
	// in clusters ("● Memcode(...)" ×N) that read as noise, so it no longer emits a tool line.

	switch cmd {
	case "overview":
		return e.introspectOverview(ctx)
	case "map":
		return e.introspectMap(ctx)
	case "context":
		return e.introspectContext(ctx, in.Target)
	case "why":
		return e.introspectWhy(ctx, in.Target)
	case "recall":
		return e.introspectRecall(ctx, in.Query, in.Limit)
	case "next":
		return e.introspectPredict(ctx)
	case "recap":
		return e.introspectRecap(ctx)
	case "memories", "claims":
		return e.introspectMemories(ctx, cmd == "memories")
	case "sources":
		return e.introspectSources(ctx)
	case "session":
		return e.introspectSession(ctx, in.Target, in.Query, in.Limit)
	case "acceptance":
		return e.introspectAcceptance(ctx)
	case "doctor":
		checks := doctor.Check(ctx, e.d.Store, e.d.Root, e.d.Prov)
		if e.d.ExtraChecks != nil {
			checks = append(checks, e.d.ExtraChecks()...)
		}
		return doctor.Render(checks), false
	case "jobs":
		if e.d.Jobs == nil {
			return "jobs unavailable in this context.", true
		}
		return e.d.Jobs(in.Target)
	case "preferences":
		return e.introspectPreferences(ctx)
	default:
		return "unknown memcode command: " + cmd + " (try: " + strings.Join(tools.MemcodeCommands, ", ") + ")", true
	}
}

// Intelligence runs a read-only memcode intelligence command (predict, overview, …)
// and returns its text — used by the TUI's "orient me" slash shortcuts. arg fills
// target/query for the commands that take one. Same code path as the agent's memcode
// tool, so the answer is identical to `memcode <command>`.
func (e *Engine) Intelligence(ctx context.Context, command, arg string) (string, bool) {
	in := tools.MemcodeInput{Command: command, Target: arg, Query: arg}
	b, _ := json.Marshal(in)
	return e.MemcodeTool(ctx, b)
}

// PersonalityBlurb asks the model for a one-line greeting in the CURRENTLY-set voice — a
// quick taste so the user immediately hears the personality they just picked. It reuses the
// chat spec (which carries the personality fact, so the doctrine composer applies the voice envelope),
// runs ONE tool-less call on a cheap model with a hard cost fuse, and is best-effort: any
// error or an empty voice returns "" and the caller simply prints nothing.
func (e *Engine) PersonalityBlurb(ctx context.Context) string {
	if e.d.Personality == "" || e.d.ChatRequest == nil {
		return ""
	}
	nctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req := e.d.ChatRequest(wire.Request{
		MaxTokens: 96,
		Messages: []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text",
			Text: "Greet me in ONE short sentence, purely in your configured voice, so I can hear the personality. No preamble, no code, no markdown headers."}}}},
	})
	resp, err := e.complete(nctx, llm.Predict, req)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(resp.Text())
}

// ArchDoc renders the architecture/flow diagrams extracted verbatim from the repo's
// docs (ARCH.md / README). Deterministic — no model, no synthesis.
func (e *Engine) ArchDoc() string { return e.redact(overview.Arch(e.d.Root)) }

// ClassifyPlanIntent moved to runtime.Session (runtime/introspect.go) — it now rides the
// shared judge plumbing (forced record_plan_intent tool, traced, failure-counted).

func (e *Engine) introspectOverview(ctx context.Context) (string, bool) {
	o, err := overview.Get(ctx, e.d.Store, e.d.Runner, e.d.Root, e.d.Model)
	if err != nil {
		return "overview failed: " + err.Error(), true
	}
	// /overview is the PROJECT overview — just that. The derived-focus block ("where
	// your head is") mostly echoed the last couple of chat lines, which read as noise;
	// the focus axis still feeds orientation/synthesis internally, it's just not
	// prepended to this display.
	return e.redact(o.Text), false
}

func (e *Engine) introspectMap(ctx context.Context) (string, bool) {
	res, err := structure.Load(ctx, e.d.Store)
	if err != nil {
		return err.Error(), true
	}
	if len(res.Subsystems) == 0 {
		return "no topology yet — `memcode init` hasn't modeled this repo.", false
	}
	totalRecent := 0
	for _, s := range res.Subsystems {
		totalRecent += s.Recent
	}
	// Rank by churn-weighted recent activity (lines changed + commits + active
	// days, last 30d) — where the work IS now, weighted by depth so many tiny
	// commits don't outrank fewer, deeper changes.
	subs := structure.ByHotness(res.Subsystems)

	var b strings.Builder
	if totalRecent > 0 {
		fmt.Fprintf(&b, "%d subsystem(s) — hottest in the last 30 days first (by churn, not just commit count):\n", len(subs))
	} else {
		fmt.Fprintf(&b, "%d subsystem(s) (no commits in the last 30 days — don't call any 'most active'):\n", len(subs))
	}
	for _, sub := range subs {
		owner := ""
		if len(sub.Owners) > 0 {
			owner = " · " + sub.Owners[0]
		}
		activity := fmt.Sprintf("%d commits", sub.Commits)
		if sub.Recent > 0 {
			activity = fmt.Sprintf("~%d lines/30d · %d recent / %d total commits · %dd active",
				sub.RecentChurn, sub.Recent, sub.Commits, sub.RecentDays)
		}
		fmt.Fprintf(&b, "- %s (%s, %s)%s\n", sub.Key, sub.Ecosystem, activity, owner)
	}
	if len(res.Deps) > 0 {
		fmt.Fprintf(&b, "dependencies:\n")
		for _, d := range res.Deps {
			fmt.Fprintf(&b, "  %s → %s\n", d.From, d.To)
		}
	}
	return b.String(), false
}

func (e *Engine) introspectContext(ctx context.Context, target string) (string, bool) {
	if target == "" {
		target = "."
	}
	pack, err := assemble.Context(ctx, e.d.Store, e.d.Root, target)
	if err != nil {
		return err.Error(), true
	}
	out, _ := json.MarshalIndent(pack, "", "  ")
	return e.redact(string(out)), false
}

func (e *Engine) introspectWhy(ctx context.Context, target string) (string, bool) {
	if target == "" {
		return "why needs a `target` (a path or subsystem).", true
	}
	prov, err := provenance.Why(ctx, e.d.Store, e.d.Root, target)
	if err != nil {
		return err.Error(), true
	}
	out, _ := json.MarshalIndent(prov, "", "  ")
	return string(out), false
}

func (e *Engine) introspectRecall(ctx context.Context, query string, limit int) (string, bool) {
	if strings.TrimSpace(query) == "" {
		return "recall needs a `query`.", true
	}
	if limit <= 0 {
		limit = 5
	}
	hits, err := recall.Recall(ctx, e.d.Store, e.d.Root, query, "", limit)
	if err != nil {
		return err.Error(), true
	}
	if len(hits) == 0 {
		return "nothing in the prose corpus matched that.", false
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "● %s (%.2f)\n%s\n\n", h.Chunk.Source, h.Score, truncate(strings.TrimSpace(h.Chunk.Text), 500))
	}
	return e.redact(b.String()), false
}

func (e *Engine) introspectPredict(ctx context.Context) (string, bool) {
	ev, err := predict.Gather(ctx, e.d.Store, e.d.Root, e.d.SessionID) // exclude the fresh current session
	if err != nil {
		return err.Error(), true
	}
	// The evidence is identical within an unchanged (HEAD, working-tree) state —
	// serve a cached synthesis instead of re-paying for the model call.
	fp := predict.Fingerprint(ctx, e.d.Root, ev)
	if cached, fresh := predict.LoadCached(ctx, e.d.Store, e.d.Root, fp); fresh {
		return cached.Text, false
	}
	usr := predict.UserPrompt(ev)
	// FAST one-shot over well-ranked evidence — NO tools (and the prompt claims none:
	// prompt capabilities must match runtime capabilities). A grounded agent loop was
	// tried and reverted (uncapped runLoop → multi-minute burn). COST FUSE: a slash
	// command must never run long or expensive — exactly ONE model call, hard 15s
	// timeout, tiny token cap.
	nctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := e.complete(nctx, llm.Predict, wire.Request{
		Mode:      "next",
		MaxTokens: 384,
		Messages:  []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: e.redact(usr)}}}},
	})
	if err != nil {
		return "predict failed: " + err.Error(), true
	}
	text := resp.Text()
	predict.StoreCached(ctx, e.d.Store, e.d.Root, fp, text)
	return text, false
}

// introspectRecap is /recap: what HAPPENED (distinct from /next = what's next).
// Same SHAPE + cost fuse as /next — fast one-shot over the same evidence (recent
// commits, uncommitted changes, where you left off), no tools, hard 15s timeout,
// small token cap. The recap doctrine (server-side) frames the evidence as a
// 3–6 bullet summary; current session if it has substance, else the last one.
func (e *Engine) introspectRecap(ctx context.Context) (string, bool) {
	ev, err := predict.Gather(ctx, e.d.Store, e.d.Root, e.d.SessionID)
	if err != nil {
		return err.Error(), true
	}
	usr := predict.UserPrompt(ev)
	nctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := e.complete(nctx, llm.Predict, wire.Request{
		Mode:      "recap",
		MaxTokens: 512,
		Messages:  []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: e.redact(usr)}}}},
	})
	if err != nil {
		return "recap failed: " + err.Error(), true
	}
	text := strings.TrimSpace(resp.Text())
	if !strings.HasPrefix(text, "recap:") {
		text = "recap: " + text
	}
	return text, false
}

func (e *Engine) introspectMemories(ctx context.Context, includeContext bool) (string, bool) {
	claims, err := e.d.Store.ListClaims(ctx)
	if err != nil {
		return err.Error(), true
	}
	var b strings.Builder
	var current, other []store.Claim
	for _, c := range claims {
		if c.Status == "current" {
			current = append(current, c)
		} else if c.Status != "rejected" {
			other = append(other, c)
		}
	}
	if len(current) == 0 && len(other) == 0 {
		return "no adjudicated claims yet — run `memcode learn`.", false
	}
	fmt.Fprintf(&b, "what memcode currently holds for this repo:\n")
	for _, c := range current {
		scope := c.Scope
		if scope == "" {
			scope = "."
		}
		fmt.Fprintf(&b, "- [%s @ %s] %s\n", c.Type, scope, c.Text)
	}
	if len(other) > 0 {
		fmt.Fprintf(&b, "\nflagged (stale/conflicted):\n")
		for _, c := range other {
			fmt.Fprintf(&b, "- [%s] %s — %s\n", c.Status, c.Type, c.Text)
		}
	}
	if includeContext {
		if recent := e.recentDecisions(ctx); recent != "" {
			fmt.Fprintf(&b, "\nrecent human decisions/notes:\n%s", recent)
		}
	}
	return e.redact(b.String()), false
}

func (e *Engine) recentDecisions(ctx context.Context) string {
	evs, err := e.d.Store.ListEvents(ctx, store.EventFilter{
		Kinds: []string{"decision", "assertion", "user_note", "frustration"},
	})
	if err != nil || len(evs) == 0 {
		return ""
	}
	if len(evs) > 8 {
		evs = evs[len(evs)-8:]
	}
	var b strings.Builder
	for _, ev := range evs {
		if t := payloadText(ev.Payload); t != "" {
			fmt.Fprintf(&b, "- (%s) %s\n", ev.Kind, truncate(t, 160))
		}
	}
	return b.String()
}

func (e *Engine) introspectSources(ctx context.Context) (string, bool) {
	srcs, err := sources.Load(ctx, e.d.Store)
	if err != nil {
		return err.Error(), true
	}
	if len(srcs) == 0 {
		return "no instruction/doc sources discovered — run `memcode init`/`memcode sources`.", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d source(s):\n", len(srcs))
	for _, src := range srcs {
		flag := ""
		if src.Stale {
			flag = " (stale)"
		}
		fmt.Fprintf(&b, "- %s [%s @ %s]%s\n", src.Path, src.Kind, orEmpty(src.Scope, "."), flag)
	}
	return b.String(), false
}

// workHistory answers "what did we work on?" — GIT-ANCHORED. The commits are the
// truth: the durable, deduplicated, ordered record of what changed, by EVERY actor
// (memcode, Claude Code, Cursor, manual git, CI), not just memcode's own loop. The
// session log is layered UNDER it as supporting color — the why and the uncommitted
// sidequests memcode itself saw, explicitly flagged as partial. Never the reverse:
// memcode's session log is one solipsistic lens; git is the shared record.
func (e *Engine) workHistory(ctx context.Context, limit int) string {
	var b strings.Builder
	commits := gitlog.Recent(ctx, e.d.Root, ".", orLimit(limit, 15))
	if len(commits) > 0 {
		b.WriteString("Recent work — git commits (the actual record, every author/tool):\n")
		for _, c := range commits {
			fmt.Fprintf(&b, "- %s  %s\n", shortSHA(c.Hash), strings.TrimSpace(c.Subject))
		}
	} else {
		b.WriteString("(no git commits found — not a git repo, or no history)\n")
	}

	// Supporting layer: memcode's OWN session notes — the why + sidequests. Partial by
	// construction (only what ran through memcode), so it's labelled as such and never
	// allowed to override the commits above.
	recs, _ := sessionlog.Recent(e.d.Root, e.d.SessionID, 0)
	threads := sessionlog.Recap(recs)
	if !sessionlog.HasActions(threads) {
		if prev, _ := sessionlog.LatestRecentExcluding(e.d.Root, e.d.SessionID, 0); len(prev) > 0 {
			threads = sessionlog.Recap(prev)
		}
	}
	if sessionlog.HasActions(threads) {
		b.WriteString("\nmemcode's own session notes (the why + sidequests — PARTIAL: only what ran through memcode; blind to Claude Code / Cursor / manual git):\n")
		b.WriteString(sessionlog.RenderRecap(threads))
	}

	// The focus projection: UNFINISHED threads from the current work burst — the
	// same window+reducer as the cold-start digest (focus.FromLog), so the tool
	// doctrine routes status questions to actually carries the open work. Before
	// this, an abandoned plan was invisible here: the digest was the only place
	// that knew, and doctrine told the model not to answer from the digest.
	var objs []objectives.Objective
	if cur, err := objectives.New(e.d.Store).Current(ctx); err == nil {
		objs = cur
	}
	if fs := focus.FromLog(e.d.Root, e.d.SessionID, objs); len(fs.Unfinished()) > 0 {
		b.WriteString("\nOPEN THREADS — unfinished work from recent sessions, newest first (the VERBATIM ask — act on the whole request, not the first line). A status answer must account for EACH (resume / done / dropped). The [from sess_…] handles are INTERNAL — session-tool targets only, never shown to the user; describe threads by their quoted ask:\n")
		for i, t := range fs.Unfinished() {
			fmt.Fprintf(&b, "%d. %s\n", i+1, t.AskLine())
		}
		if fs.Elided > 0 {
			fmt.Fprintf(&b, "…and %d older thread(s) beyond the window — memcode{command:\"session\", target:\"recent\"} lists every session.\n", fs.Elided)
		}
	}
	return b.String()
}

func shortSHA(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func orLimit(n, def int) int {
	if n <= 0 || n > 50 {
		return def
	}
	return n
}

// introspectSession is the agent's self-recall primitive over the episodic log
// (.memcode/sessions/<id>/). Subcommands: recap (default) | previous | recent |
// search <query> | commits | sidequests | shell. recap shows this session's
// checklist — or, when this session hasn't acted yet, the previous one; `previous`
// forces the last distinct session; `shell` returns the last `$` command + its
// output + exit (for explain/fix-last). This is the source of truth for
// "what did I/we do earlier?" — the agent consults it instead of guessing from
// context residue.
func (e *Engine) introspectSession(ctx context.Context, sub, query string, limit int) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(sub)) {
	case "", "recap": // default: "what did we work on" — GIT is truth, session log supports
		return e.redact(e.workHistory(ctx, limit)), false
	case "previous", "last", "prior": // the previous distinct session, explicitly
		recs, err := sessionlog.LatestRecentExcluding(e.d.Root, e.d.SessionID, 0)
		if err != nil {
			return err.Error(), true
		}
		if len(recs) == 0 {
			return "no earlier session recorded yet — this is the first one.", false
		}
		return e.redact(sessionlog.RenderRecap(sessionlog.Recap(recs))), false
	case "current", "this": // the ACTIVE session's activity
		if limit <= 0 {
			limit = 30
		}
		recs, err := sessionlog.Recent(e.d.Root, e.d.SessionID, limit)
		if err != nil {
			return err.Error(), true
		}
		return e.formatSessionRecords(recs, "nothing recorded in this session yet.")
	case "recent", "sessions", "list": // the last N DISTINCT sessions (not the current one)
		return e.recentSessionsList(limit)
	case "shell", "last_shell", "last-command": // explain/fix-last: the most recent `$` command + result
		r, ok := sessionlog.LastShell(e.d.Root)
		if !ok {
			return "no `$` shell command has been run yet.", false
		}
		status := "exit 0"
		if r.IsError {
			status = fmt.Sprintf("exit %d (failed)", r.Exit)
		}
		out := strings.TrimSpace(r.Content)
		if out == "" {
			out = "(no output)"
		}
		return e.redact(fmt.Sprintf("last `$` command:\n$ %s\n%s\n\n%s", r.Input, status, out)), false
	case "search":
		if strings.TrimSpace(query) == "" {
			return "session search needs a `query`.", true
		}
		if limit <= 0 {
			limit = 20
		}
		recs, err := sessionlog.Search(e.d.Root, query, limit)
		if err != nil {
			return err.Error(), true
		}
		return e.formatSessionRecords(recs, "nothing in the session history matched that.")
	case "commits":
		commits := gitlog.Recent(ctx, e.d.Root, ".", orLimit(limit, 20))
		if len(commits) == 0 {
			return "no git commits found (not a git repo, or no history).", false
		}
		var b strings.Builder
		b.WriteString("git commits — the actual record, every author/tool:\n")
		for _, c := range commits {
			fmt.Fprintf(&b, "- %s  %s\n", shortSHA(c.Hash), strings.TrimSpace(c.Subject))
		}
		return e.redact(b.String()), false
	case "sidequests":
		recs, err := sessionlog.Sidequests(e.d.Root, e.d.SessionID)
		if err != nil {
			return err.Error(), true
		}
		return e.formatSessionRecords(recs, "no user requests recorded in this session yet.")
	}
	return e.recentSessionsList(limit) // unknown sub → the recent-sessions list
}

// recentSessionsList renders the last N distinct prior sessions (newest first) with
// each session's headline (its first user request) — the substrate for "what were
// the last couple sessions about?". The single session-list path.
func (e *Engine) recentSessionsList(limit int) (string, bool) {
	if limit <= 0 {
		limit = 6
	}
	sums, err := sessionlog.RecentSessions(e.d.Root, e.d.SessionID, limit)
	if err != nil {
		return err.Error(), true
	}
	if len(sums) == 0 {
		return "no earlier sessions recorded yet — this is the first.", false
	}
	var b strings.Builder
	b.WriteString("recent sessions (newest first):\n")
	for i, su := range sums {
		actions := ""
		if su.Actions > 0 {
			actions = fmt.Sprintf("  · %d action(s)", su.Actions)
		}
		fmt.Fprintf(&b, "%d. %s — %s%s\n", i+1, timeAgo(su.Started), su.Headline, actions)
	}
	return e.redact(strings.TrimRight(b.String(), "\n")), false
}

// formatSessionRecords renders episodic-log records as a compact, redacted,
// human-readable list (newest entries as written; one line each).
func (e *Engine) formatSessionRecords(recs []sessionlog.Record, empty string) (string, bool) {
	if len(recs) == 0 {
		return empty, false
	}
	var b strings.Builder
	for _, r := range recs {
		ts := r.TS.Local().Format("Jan 2 15:04")
		switch r.Kind {
		case sessionlog.KindUserMessage:
			fmt.Fprintf(&b, "%s  › %s\n", ts, clipLine(r.Text, 200))
		case sessionlog.KindAssistantMessage:
			fmt.Fprintf(&b, "%s  memcode: %s\n", ts, clipLine(r.Text, 200))
		case sessionlog.KindToolCall:
			fmt.Fprintf(&b, "%s  ⏺ %s %s\n", ts, r.Tool, clipLine(r.Input, 140))
		case sessionlog.KindApproval:
			fmt.Fprintf(&b, "%s  ✓ %s: %s\n", ts, r.Decision, clipLine(r.Text, 140))
		case sessionlog.KindToolResult:
			mark := "⎿"
			if r.IsError {
				mark = "⚠"
			}
			fmt.Fprintf(&b, "%s  %s %s\n", ts, mark, clipLine(r.Content, 140))
		case sessionlog.KindCompaction:
			fmt.Fprintf(&b, "%s  ⊙ context compacted — %s\n", ts, clipLine(r.Text, 140))
		case sessionlog.KindSessionStarted:
			fmt.Fprintf(&b, "%s  ▸ session start (model %s, %s mode)\n", ts, r.Model, r.Mode)
		case sessionlog.KindSessionFinished:
			fmt.Fprintf(&b, "%s  ■ session end\n", ts)
		}
	}
	return e.redact(strings.TrimRight(b.String(), "\n")), false
}

// clipLine collapses to the first line and caps the length for one-row display.
func clipLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > max {
		s = textutil.ClipBytes(s, max) + "…" // rune-safe: never split a multibyte char
	}
	return s
}

func (e *Engine) introspectAcceptance(ctx context.Context) (string, bool) {
	_, _ = acceptance.Reconcile(ctx, e.d.Store, e.d.Root)
	evs, err := e.d.Store.ListEvents(ctx, store.EventFilter{Kinds: []string{string(events.KindSessionOutcome)}})
	if err != nil {
		return err.Error(), true
	}
	if len(evs) == 0 {
		return "no reconciled session outcomes yet (run the agent, then commit/revert its work).", false
	}
	var b strings.Builder
	for _, ev := range evs {
		var m map[string]any
		_ = json.Unmarshal(ev.Payload, &m)
		fmt.Fprintf(&b, "- %s → %s — %s\n", str2(m["session_id"]), str2(m["outcome"]), str2(m["evidence"]))
	}
	return b.String(), false
}

// introspectPreferences lists the user's learned preferences in three tiers:
// confirmed (standing rules in .memcode/prefs/*.md), candidates (accumulating
// evidence, not yet promoted), and demoted (overturned by contradiction). This
// is the review surface for the evidence-weighted preference system — the user
// sees what the system learned, with evidence, and can edit/delete the plaintext
// files directly to revert.
func (e *Engine) introspectPreferences(ctx context.Context) (string, bool) {
	cands, err := e.d.Store.ListPreferenceCandidates(ctx)
	if err != nil {
		return "preferences failed: " + err.Error(), true
	}

	var b strings.Builder
	any := false

	// Confirmed: standing rules with their file paths.
	var confirmed, candidates, demoted []store.PreferenceCandidate
	for _, c := range cands {
		switch c.Status {
		case "confirmed":
			confirmed = append(confirmed, c)
		case "demoted":
			demoted = append(demoted, c)
		default:
			candidates = append(candidates, c)
		}
	}

	if len(confirmed) > 0 {
		any = true
		b.WriteString("CONFIRMED standing preferences (binding — edit/delete the file to revert):\n")
		for _, c := range confirmed {
			fmt.Fprintf(&b, "- [%s] %s (weight %.2f, %d signals/%d sessions)\n", c.Axis, c.Text, c.Weight, c.SignalCount, c.SessionCount)
			if c.ConfirmedPath != "" {
				fmt.Fprintf(&b, "  → %s\n", c.ConfirmedPath)
			}
		}
	}
	if len(candidates) > 0 {
		if any {
			b.WriteString("\n")
		}
		any = true
		b.WriteString("CANDIDATES (accumulating evidence — promote at weight ≥ 2.0, ≥3 signals, ≥2 sessions):\n")
		for _, c := range candidates {
			fmt.Fprintf(&b, "- [%s] %s (weight %.2f, %d signals/%d sessions)\n", c.Axis, c.Text, c.Weight, c.SignalCount, c.SessionCount)
		}
	}
	if len(demoted) > 0 {
		if any {
			b.WriteString("\n")
		}
		any = true
		b.WriteString("DEMOTED (overturned by contradiction):\n")
		for _, c := range demoted {
			fmt.Fprintf(&b, "- [%s] %s (was weight %.2f)\n", c.Axis, c.Text, c.Weight)
		}
	}
	if !any {
		return "no preference candidates yet — the system learns from your repeated forceful directives (\"always X\", \"never Y\").", false
	}
	return b.String(), false
}

// --- helpers ---

// truncate caps a long block with a trailing ellipsis line (introspect's own copy;
// runtime has an identical unexported one for its own use).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return textutil.ClipBytes(s, n) + "\n…(truncated)" // rune-safe byte-budget cut
}

// orEmpty returns fallback when s is empty.
func orEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func payloadText(p json.RawMessage) string {
	var m map[string]any
	if len(p) == 0 || json.Unmarshal(p, &m) != nil {
		return ""
	}
	for _, k := range []string{"text", "note", "message", "decision", "title", "reason"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func str2(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// timeAgo renders a timestamp the way a human refers to it — "just now", "20m ago",
// "3h ago" for today's work — falling back to an absolute date only once it's old
// enough that the calendar matters.
func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Local().Format("Jan 2")
	}
}
