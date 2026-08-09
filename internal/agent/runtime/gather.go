package runtime

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/llm"
)

// Gather telemetry OBSERVES a turn's read-only INFORMATION GATHERING — it records how much
// reading a turn did (feeding KindGatherSummary, which the substrate /analyze reads). It does
// NOT nudge the model and does NOT block or skip any tool call: deciding when it has read
// enough is the MODEL's job, not the runtime's. We tried both being clever about it — a
// "you've gathered enough, act now" nudge and a "skipped — already gathered this" re-read
// short-circuit — and both outsmarted the model (the nudge produced confident-but-under-
// researched turns; the skip blocked legitimate re-reads of state that had changed). Both
// removed. We count ONLY read-only gather tools (grep/read/list/search/fetch/explore); edits,
// builds, and tests aren't counted. The mode-aware budgets below are now purely the
// over_budget THRESHOLD for that telemetry — crossing one no longer does anything.
const (
	applyGatherBudget  = 20 // apply: the plan is the contract — reads expected to be sparing
	planGatherBudget   = 60 // plan: research IS the deliverable — looser
	scoutGatherBudget  = 50 // scout/explorer: gathering IS its job — generous
	reviewGatherBudget = 12 // plan-review sanity gate: a few spot-check reads, not a re-audit
	chatGatherBudget   = 40 // normal chat/edit — in between
	gatherRepeatLimit  = 3  // reads of ONE target beyond this are re-reads, not new facts
)

type gatherState struct {
	total    int
	byTarget map[string]int
}

func newGatherState() *gatherState {
	return &gatherState{byTarget: map[string]int{}}
}

// gatherMode names the active turn mode and its read-only-gathering ceiling. apply is
// tightest (the plan is the contract); plan is loosest (research IS the deliverable);
// scout is a read-only explorer sub-agent (gathering is its job).
func (s *Session) gatherMode() (mode string, budget int) {
	switch {
	case s.readOnly && s.purpose == llm.Review: // the bounded plan-review audit sub-session
		return "review", reviewGatherBudget
	case s.readOnly: // a scout/explorer sub-agent (readOnly is set only in Answer)
		return "scout", scoutGatherBudget
	case s.planActiveOrApplying(): // plan-mode state (nil-safe: tests construct Session without planCtl)
		if s.planCtl.IsApplying() {
			return "apply", applyGatherBudget
		}
		return "plan", planGatherBudget
	default:
		return "chat", chatGatherBudget
	}
}

// planActiveOrApplying is a nil-safe check for plan-mode state — tests that construct
// Session{} directly (without going through New) have a nil planCtl.
func (s *Session) planActiveOrApplying() bool {
	return s.planCtl != nil && (s.planCtl.IsApplying() || s.planCtl.Planning())
}

// gatherBudget is the mode-aware read-only-gathering ceiling — now purely the over_budget
// threshold for the gather telemetry (nothing fires when it's crossed).
func (s *Session) gatherBudget() int { _, b := s.gatherMode(); return b }

// summary is the per-turn gather telemetry payload (KindGatherSummary): total reads,
// distinct targets, the mode budget, whether it ran over, and the targets read more than
// once (the re-read signal — the SDK-migration turn re-read one SDK file ~40×).
// Deterministic; the substrate /analyze reads it to judge efficiency.
func (g *gatherState) summary(mode string, budget int) map[string]any {
	repeats := map[string]int{}
	overLimit := 0
	for t, n := range g.byTarget {
		if n > 1 {
			repeats[shortTarget(t)] = n // only re-reads matter; first read of a target is just work
		}
		if n > gatherRepeatLimit {
			overLimit++
		}
	}
	return map[string]any{
		"mode":            mode,
		"reads":           g.total,
		"distinct":        len(g.byTarget),
		"budget":          budget,
		"over_budget":     g.total > budget,
		"repeats":         repeats,   // re-read target -> count
		"over_repeat_lim": overLimit, // distinct targets read beyond gatherRepeatLimit
	}
}

// noteGather records read-only gather TELEMETRY — total reads + per-target counts, which feed
// the KindGatherSummary the substrate /analyze reads. It NEVER blocks or short-circuits a tool
// call: the model decides when it has read enough, and re-running a read/bash whose output
// legitimately changed (git status after an edit, a re-test, ls after a write) must always
// work. (Removed the "skipped — already gathered this" short-circuit — it was outsmarting the
// model and blocked legitimate re-reads of state that had changed.)
func (s *Session) noteGather(name string, input json.RawMessage) {
	if s.turn.gather == nil {
		return
	}
	target, ok := gatherSignature(name, input)
	if !ok {
		return
	}
	// Read-modify-write on the gather counters MUST be atomic: noteGather is called from the
	// concurrent executeBatch goroutines (parallel read-only tools all hit this), so an
	// unguarded map write here is a `concurrent map writes` fatal. Guard with the same s.mu the
	// rest of the concurrent path (metrics counters, readHashes) already uses.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn.gather.total++
	s.turn.gather.byTarget[target]++
}

// gatherSignature returns (normalized-target, true) when a tool call is read-only
// information gathering; ("", false) for productive work (edits, builds, tests, todos,
// asks) and for git diff (that's verification, not archaeology).
func gatherSignature(name string, input json.RawMessage) (string, bool) {
	switch name {
	case tools.ReadFile:
		return "read:" + jsonField(input, "path"), true
	case tools.ListDir:
		return "ls:" + jsonField(input, "path"), true
	case tools.Glob:
		return "glob:" + jsonField(input, "pattern"), true
	case tools.Ripgrep:
		return "rg:" + jsonField(input, "path"), true // the FILE/scope is the target; the pattern varies
	case tools.CodeQuery:
		return "codeq:" + jsonField(input, "query"), true
	case tools.Memcode:
		return "memcode:" + jsonField(input, "target"), true
	case tools.Explore:
		return "explore:" + jsonField(input, "scope"), true
	case tools.WebSearch:
		return "websearch", true
	case tools.Fetch:
		return "fetch:" + jsonField(input, "url"), true
	case tools.Trace:
		return "trace", true
	case tools.Bash:
		return bashGatherTarget(jsonField(input, "command"))
	}
	return "", false
}

// bashGatherTarget classifies a bash command: a read-only SOURCE READ (grep/sed/cat/…)
// counts as gather, keyed by the file path(s) it touches; anything else (go build/test,
// git, make, mkdir, …) is productive and returns ("", false).
func bashGatherTarget(cmd string) (string, bool) {
	switch firstWord(cmd) { // firstWord skips leading VAR=val env assignments
	case "grep", "rg", "egrep", "fgrep", "sed", "awk", "cat", "head", "tail", "less", "more", "nl", "find", "ls", "wc":
		return "bash:" + bashPaths(cmd), true
	}
	return "", false
}

// bashPaths extracts the path-like tokens a read command references, sorted+joined, so the
// SAME file read with different patterns/line-ranges maps to ONE target (that's the
// re-read we want to catch). Falls back to the command head when no path is present.
func bashPaths(cmd string) string {
	var paths []string
	for _, tok := range strings.Fields(cmd) {
		tok = strings.Trim(tok, "'\"`")
		if strings.Contains(tok, "/") && !strings.HasPrefix(tok, "-") {
			paths = append(paths, tok)
		}
	}
	if len(paths) == 0 {
		return clip(cmd, 40)
	}
	sort.Strings(paths)
	return strings.Join(paths, ",")
}

// jsonField pulls a single string field out of a tool's raw input.
func jsonField(input json.RawMessage, field string) string {
	var m map[string]any
	if json.Unmarshal(input, &m) == nil {
		if v, ok := m[field].(string); ok {
			return v
		}
	}
	return ""
}

// shortTarget trims a target key to its tail (the filename) for a readable nudge.
func shortTarget(t string) string {
	if i := strings.LastIndexByte(t, '/'); i >= 0 && i < len(t)-1 {
		return t[i+1:]
	}
	return clip(t, 50)
}
