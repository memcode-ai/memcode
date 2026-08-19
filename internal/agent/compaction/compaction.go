// Package compaction is the PURE, testable core of in-session context compaction:
// deciding WHERE to cut a running conversation and rendering the cut-off part into
// a plain transcript for an Anthropic summarizer. It owns no model call and no I/O
// — the runtime orchestrates those — so the load-bearing invariant (never split a
// tool_use from its tool_result) can be proven in isolation.
//
// The doctrine (three layers — hot raw turns / warm summary / cold session log)
// lives in COMPACTION.md. This package implements the warm layer's mechanics.
package compaction

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/memcode-ai/memcode/internal/textutil"
	"github.com/memcode-ai/memcode/internal/wire"
)

// charsPerToken is the coarse heuristic shared with the gateway's pre-call size
// estimate (~4 chars/token over message text). Exactness doesn't matter — the
// compaction budget is a soft threshold and an over-estimate just compacts a turn
// early (the safe direction).
const charsPerToken = 4

// EstimateTokens approximates the prompt size of a message slice the way the
// gateway's router does: ~4 chars/token over all text and tool-result content.
func EstimateTokens(messages []wire.Message) int {
	n := 0
	for _, m := range messages {
		for _, b := range m.Blocks {
			n += len(b.Text) + len(b.Content) + len(b.Input) + len(b.Thinking)
			// A tool_result's flat Content mirrors its ContentBlocks text (counting
			// both would double-count), but a structured-ONLY result — an image
			// block, no flat text — was invisible here, hiding screenshots from
			// every budget decision. Count nested payloads when Content is empty.
			if b.Content == "" {
				for _, cb := range b.ContentBlocks {
					n += len(cb.Text)
					if cb.Source != nil {
						n += len(cb.Source.Data) / 2 // base64 image data still costs real tokens
					}
				}
			}
		}
	}
	return n / charsPerToken
}

// isTurnStart reports whether a message BEGINS a new user turn — a user message
// that carries author-written content (text/image/document), NOT a carrier of
// tool_result blocks (which belong to the assistant's prior turn). Turn starts are
// the ONLY safe split boundaries: everything between two of them is exactly one
// complete turn, so cutting there can never separate a tool_use from its result.
func isTurnStart(m wire.Message) bool {
	if m.Role != "user" {
		return false
	}
	for _, b := range m.Blocks {
		if b.Type == "tool_result" {
			continue
		}
		return true // any author-written block (text/image/document)
	}
	return false
}

// turnStarts returns the indices of every real user-turn boundary in order.
func turnStarts(messages []wire.Message) []int {
	var idx []int
	for i, m := range messages {
		if isTurnStart(m) {
			idx = append(idx, i)
		}
	}
	return idx
}

// Plan splits a conversation into a HEAD to summarize and a TAIL to keep verbatim,
// cutting at a user-turn boundary so the last keepRecent turns survive raw and no
// tool_use/tool_result pair is ever divided. ok is false when the session is still
// short enough that there's nothing worth compacting (≤ keepRecent turns).
func Plan(messages []wire.Message, keepRecent int) (head, tail []wire.Message, ok bool) {
	if keepRecent < 1 {
		keepRecent = 1
	}
	starts := turnStarts(messages)
	if len(starts) <= keepRecent {
		return nil, nil, false // not enough completed turns to compact yet
	}
	split := starts[len(starts)-keepRecent] // first index of the kept tail
	if split <= 0 {
		return nil, nil, false
	}
	return messages[:split], messages[split:], true
}

// CountTurns reports how many user-turn boundaries a message slice contains — used
// for the "summarized N earlier turns" telemetry and status line.
func CountTurns(messages []wire.Message) int {
	return len(turnStarts(messages))
}

// toolOutputClip caps how much of a single tool result survives into the
// transcript. Tool output (grep/test/build dumps) bloats fastest and matters least
// to the summary — clip it hard; the model keeps the gist, not the firehose.
const toolOutputClip = 220

// proseClip caps a single user/assistant text block. Generous — the actual prose
// of the conversation is what we most want the summarizer to see.
const proseClip = 4000

// Render flattens the head messages into a plain-text transcript for the
// summarizer. It is deliberately lossy in the safe direction: tool mechanics are
// reduced to a one-liner and tool output is clipped, while user/assistant prose is
// preserved nearly whole. The output carries NO doctrine — it is pure session
// content; the compaction prompt lives server-side.
func Render(messages []wire.Message) string {
	var b strings.Builder
	for _, m := range messages {
		for _, blk := range m.Blocks {
			switch blk.Type {
			case "text":
				if t := strings.TrimSpace(blk.Text); t != "" {
					who := "User"
					if m.Role == "assistant" {
						who = "Assistant"
					}
					fmt.Fprintf(&b, "%s: %s\n", who, clip(t, proseClip))
				}
			case "image", "document":
				fmt.Fprintf(&b, "User: [attached %s]\n", blk.Type)
			case "tool_use":
				fmt.Fprintf(&b, "Assistant called %s(%s)\n", blk.Name, clip(strings.TrimSpace(string(blk.Input)), toolOutputClip))
			case "tool_result":
				mark := "result"
				if blk.IsError {
					mark = "error"
				}
				if c := strings.TrimSpace(blk.Content); c != "" {
					fmt.Fprintf(&b, "  (%s: %s)\n", mark, clip(c, toolOutputClip))
				}
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// OmittedToolResult is the LEGACY blank placeholder; offloadRef now writes a TYPED pointer back to
// the source instead (below). Kept so isOffloaded still recognizes results elided by older builds.
const OmittedToolResult = "[earlier tool output omitted to keep this turn within the context window]"

// offloadPrefix marks a tool_result whose payload has been replaced by a compact pointer, so the
// eviction pass can tell an already-offloaded block from a live one and never re-process it.
const offloadPrefix = "[offloaded — "

// isOffloaded reports whether a tool_result's content is already a pointer (the typed form or the
// legacy blank), not a live payload.
func isOffloaded(content string) bool {
	return content == OmittedToolResult || strings.HasPrefix(content, offloadPrefix)
}

// offloadRef renders a tool_result as a compact, TYPED pointer back to its source instead of its
// full payload — so older outputs stop riding the context window every iteration. A file read is
// already on disk, so the pointer just names the path ("re-read to restore") — fully lossless.
// Other outputs name how to re-fetch. name/input come from the matching tool_use (by tool_use_id).
func offloadRef(name string, input json.RawMessage) string {
	switch name {
	case "read_file":
		if p := jsonStr(input, "path"); p != "" {
			if a, b := jsonInt(input, "start_line"), jsonInt(input, "end_line"); a > 0 || b > 0 {
				return fmt.Sprintf("%sread_file %s lines %d-%d — content is on disk; re-read that range to restore it]", offloadPrefix, p, a, b)
			}
			return offloadPrefix + "read_file " + p + " — content is on disk; re-read the file to restore it]"
		}
	case "ripgrep", "glob", "list_dir", "code_query", "git_diff":
		if a := jsonStr(input, "pattern", "query", "path"); a != "" {
			return offloadPrefix + name + " " + a + " — re-run to restore]"
		}
		return offloadPrefix + name + " output — re-run to restore]"
	case "bash":
		if c := jsonStr(input, "command"); c != "" {
			return offloadPrefix + "bash `" + clipArg(c, 80) + "` output — re-run to restore]"
		}
	}
	if name != "" {
		return offloadPrefix + name + " output omitted]"
	}
	return OmittedToolResult
}

// jsonInt returns an integer field from a tool_use input object (0 when absent).
func jsonInt(input json.RawMessage, key string) int {
	if len(input) == 0 {
		return 0
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return 0
	}
	if f, ok := m[key].(float64); ok {
		return int(f)
	}
	return 0
}

// jsonStr returns the first present non-empty string field among keys in a tool_use input object.
func jsonStr(input json.RawMessage, keys ...string) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func clipArg(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > max {
		return string([]rune(s)[:max-1]) + "…"
	}
	return s
}

// EvictOpts tunes EvictStaleToolResultsOpts beyond the keep count.
type EvictOpts struct {
	// Pinned reports whether a read_file path is HOT — the session has observed
	// it being re-read (typically right after a prior eviction), so evicting it
	// again just forces another re-read (the measured read-evict-reread thrash:
	// the same 9 files re-read 13-14x in one session). A pinned path's LATEST
	// live read survives even past the keepRecent cutoff. Superseded duplicates
	// of a pinned path still offload — that is deduplication, not thrash.
	Pinned func(path string) bool
}

// EvictStaleToolResults offloads the payload of OLD tool_result blocks — replacing each with a TYPED
// pointer (offloadRef) back to its source — so a long turn stops re-sending stale grep/read/test
// dumps every iteration. It is RELEVANCE-FIRST, not purely age-based:
//   - a SUPERSEDED read (an earlier read of a file that's read AGAIN later) is offloaded eagerly,
//     even if recent — the newer copy is authoritative, so the older is dead weight; and
//   - among what's left, the most recent keepRecent stay verbatim (the active working set).
//
// A file read's pointer is lossless (the file is on disk; re-read to restore); other outputs name
// how to re-fetch. It NEVER removes a block — only swaps payload for a pointer — so tool_use↔
// tool_result adjacency holds. Returns how many it offloaded.
func EvictStaleToolResults(messages []wire.Message, keepRecent int) int {
	return EvictStaleToolResultsOpts(messages, keepRecent, EvictOpts{})
}

// EvictStaleToolResultsOpts is EvictStaleToolResults with options (hot-path pinning).
func EvictStaleToolResultsOpts(messages []wire.Message, keepRecent int, opts EvictOpts) int {
	if keepRecent < 0 {
		keepRecent = 0
	}
	// tool_use id → its call, so each evicted result points precisely back to what produced it.
	calls := map[string]wire.Block{}
	for mi := range messages {
		for bi := range messages[mi].Blocks {
			if b := messages[mi].Blocks[bi]; b.Type == "tool_use" && b.ID != "" {
				calls[b.ID] = b
			}
		}
	}
	type loc struct {
		m, b       int
		superseded bool
	}
	var locs []loc
	for mi := range messages {
		for bi := range messages[mi].Blocks {
			bl := messages[mi].Blocks[bi]
			// Structured-only results (empty Content but ContentBlocks set — e.g.
			// screenshots) are candidates too; they were the LARGEST blocks and the
			// old Content-only check skipped them entirely.
			if bl.Type == "tool_result" && !isOffloaded(bl.Content) &&
				(bl.Content != "" || len(bl.ContentBlocks) > 0) {
				locs = append(locs, loc{m: mi, b: bi})
			}
		}
	}
	// Mark superseded reads: the LAST live read for a given path+range wins; earlier reads of the
	// SAME slice are stale the moment it's read again. Ranges are distinct working-set members —
	// reading lines 500-600 must NOT supersede a live read of lines 1-100 — but a later FULL read
	// covers every range of that path and supersedes them all.
	readPath := func(l loc) string {
		c := calls[messages[l.m].Blocks[l.b].ToolUseID]
		if c.Name != "read_file" {
			return ""
		}
		return jsonStr(c.Input, "path")
	}
	readKey := func(l loc) string {
		c := calls[messages[l.m].Blocks[l.b].ToolUseID]
		if c.Name != "read_file" {
			return ""
		}
		p := jsonStr(c.Input, "path")
		if p == "" {
			return ""
		}
		if a, b := jsonInt(c.Input, "start_line"), jsonInt(c.Input, "end_line"); a > 0 || b > 0 {
			return fmt.Sprintf("%s\x00%d-%d", p, a, b)
		}
		return p
	}
	lastRead := map[string]int{} // path or path\x00range → newest live read of that slice
	lastFull := map[string]int{} // path → newest live FULL read
	for i, l := range locs {
		if k := readKey(l); k != "" {
			lastRead[k] = i
			if !strings.ContainsRune(k, '\x00') {
				lastFull[k] = i
			}
		}
	}
	for i := range locs {
		k := readKey(locs[i])
		if k == "" {
			continue
		}
		if lastRead[k] != i {
			locs[i].superseded = true
			continue
		}
		if strings.ContainsRune(k, '\x00') { // a range read — a LATER full read covers it
			if fi, ok := lastFull[readPath(locs[i])]; ok && fi > i {
				locs[i].superseded = true
			}
		}
	}
	// Keep the most recent keepRecent NON-superseded raw (the active working set); offload the
	// rest — except the latest live read of a PINNED (hot) path, which survives the cutoff.
	kept, n := 0, 0
	for i := len(locs) - 1; i >= 0; i-- {
		l := locs[i]
		if !l.superseded {
			if kept < keepRecent {
				kept++
				continue
			}
			if opts.Pinned != nil {
				if p := readPath(l); p != "" && opts.Pinned(p) {
					continue
				}
			}
		}
		bl := &messages[l.m].Blocks[l.b]
		c := calls[bl.ToolUseID]
		bl.Content = offloadRef(c.Name, c.Input)
		// Drop the structured payload too: every provider PREFERS ContentBlocks
		// when present, so leaving it made this whole eviction a wire no-op —
		// the pointer went in Content while the full dump still shipped.
		bl.ContentBlocks = nil
		n++
	}
	return n
}

// clip collapses whitespace-runs lightly and truncates s to at most n bytes with a
// single-line ellipsis, so one transcript entry stays compact. Rune-safe cut: a
// byte slice could split a multibyte rune and emit invalid UTF-8.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return textutil.ClipBytes(s, n) + "…"
}
