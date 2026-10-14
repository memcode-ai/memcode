// Package codex is an observe.Adapter over Codex's own local session
// transcripts (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl — or
// $CODEX_HOME/sessions if that's set, the officially documented override:
// developers.openai.com/codex/config-reference). The format is NOT
// versioned or documented for third-party readers; everything this file
// assumes was confirmed against a live sample at implementation time (see
// the field comments below), including the session's recorded cwd field
// used for repo-root filtering. Parsing is defensive by construction — a
// line, or a line's shape, this package doesn't recognize is skipped,
// never fatal; if a future format change moves/renames the cwd field, cwd
// filtering degrades to "found nothing" (Discover returns no files), never
// a crash.
//
// Deliberately narrow in what it extracts — same reasoning as package
// claudecode's doc comment (cost: no raw transcript text ever reaches
// disk; fidelity to memcode's own "meaningful action" doctrine). Confirmed
// against a live sample: Codex's own file-edit tool calls all go through a
// shell invocation (exec_command) rather than a structured
// full-new-content field, so — unlike Claude Code's Write — there is no
// FileClaim this importer can recover without replaying a diff against the
// live filesystem, which an importer must never do. Only a human's own ask
// and a real `git commit`/`push` become a Record.
package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/observe"
	"github.com/memcode-ai/memcode/internal/sessionlog"
)

// Source is this adapter's provenance tag (focus.Turn.Source and the
// .memcode/observe/<source>/ directory name).
const Source = "codex"

// Importer is the zero-value observe.Adapter implementation — stateless,
// every method a pure function of its arguments.
type Importer struct{}

func (Importer) Source() string { return Source }

func (Importer) Discover(repoRoots []string) []string { return Discover(repoRoots) }

func (Importer) Import(path string, fromOffset, maxBytes int64) ([]observe.Record, int64, error) {
	return Import(path, fromOffset, maxBytes)
}

// baseDir returns Codex's session root: $CODEX_HOME/sessions (the
// officially documented override) if CODEX_HOME is set, else
// ~/.codex/sessions.
func baseDir() (string, bool) {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return filepath.Join(d, "sessions"), true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".codex", "sessions"), true
}

// maxAge bounds which transcripts Discover even looks at — same "stay
// efficient, look at recent things" rule as package claudecode, and the
// tightest bound in this whole feature: Codex rollout files have been
// observed in the wild up to ~2GB, so never even LOOKING at a stale one
// matters more here than anywhere else.
const maxAge = 7 * 24 * time.Hour

// Discover locates every Codex rollout whose session cwd matches one of
// repoRoots (the project(s) under this workspace), modified within the
// last maxAge. Rollouts are laid out by calendar date
// (sessions/YYYY/MM/DD/), so only the last maxAge's worth of day
// directories are even listed — never a walk over the user's entire Codex
// history.
func Discover(repoRoots []string) []string {
	base, ok := baseDir()
	if !ok {
		return nil
	}
	roots := map[string]bool{}
	for _, r := range repoRoots {
		roots[r] = true
	}
	cutoff := time.Now().Add(-maxAge)
	var files []string
	for _, dir := range candidateDayDirs(base, cutoff) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "rollout-") || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().Before(cutoff) {
				continue // stale (or unreadable) — not "where you left off"
			}
			path := filepath.Join(dir, e.Name())
			if cwd, ok := peekCWD(path); ok && roots[cwd] {
				files = append(files, path)
			}
		}
	}
	sort.Strings(files)
	return files
}

// candidateDayDirs returns base/YYYY/MM/DD for every calendar day from
// cutoff through tomorrow (a one-day pad for timezone slop between this
// machine's local day and whatever Codex stamped), never the whole tree.
func candidateDayDirs(base string, cutoff time.Time) []string {
	var dirs []string
	start := cutoff.Truncate(24 * time.Hour)
	end := time.Now().AddDate(0, 0, 1)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		dirs = append(dirs, filepath.Join(base,
			strconv.Itoa(d.Year()), pad2(int(d.Month())), pad2(d.Day())))
	}
	return dirs
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// peekCWD reads just enough of a rollout to find its session_meta entry
// (confirmed, against a live sample, to carry the session's cwd — and to
// appear within the first several lines) and returns payload.cwd. Bounded:
// gives up after peekLines lines, never scans a whole multi-GB rollout just
// to filter it.
const peekLines = 50

func peekCWD(path string) (string, bool) {
	lines, _, err := observe.ReadNewLines(path, 0, 1<<20) // 1 MiB is far more than peekLines needs
	if err != nil {
		return "", false
	}
	for i, line := range lines {
		if i >= peekLines {
			break
		}
		var env envelope
		if json.Unmarshal(line, &env) != nil || env.Type != "session_meta" {
			continue
		}
		var meta struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(env.Payload, &meta) == nil && meta.CWD != "" {
			return meta.CWD, true
		}
	}
	return "", false
}

// envelope is every rollout line's outer shape: {timestamp, type, payload}.
// Only a few of the real "type"s carry anything this importer reads
// (session_meta for cwd; event_msg/response_item for conversation content);
// everything else (turn_context, world_state, compacted, …) is skipped for
// free by never matching a case below.
type envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// eventMsgPayload covers event_msg's "user_message" shape — the real human
// input (as opposed to response_item's "message" entries, which also
// include developer/app-context boilerplate that is NOT the user's ask).
type eventMsgPayload struct {
	Type    string `json:"type"` // "user_message" | "agent_message" | "task_started" | "token_count" | …
	Message string `json:"message"`
}

// responseItemPayload covers response_item's "function_call" shape — a
// tool invocation. Codex's shell tool is named "exec_command"; its
// arguments are a JSON-encoded string (not a nested object) carrying "cmd".
type responseItemPayload struct {
	Type      string `json:"type"` // "message" | "function_call" | "function_call_output" | "reasoning" | …
	Name      string `json:"name"` // function_call: tool name ("exec_command", "write_stdin", …)
	Arguments string `json:"arguments"`
}

type execCommandArgs struct {
	Cmd string `json:"cmd"`
}

// Import parses the new lines appended to path since fromOffset (via
// observe.ReadNewLines, which bounds how much is read and guarantees a line
// is never split across two calls), up to maxBytes of new content, and
// returns the Records extracted plus the offset to resume from next time. A
// line that fails to parse, or whose shape isn't one this importer
// recognizes, is skipped — never fatal to the rest of the file.
func Import(path string, fromOffset, maxBytes int64) ([]observe.Record, int64, error) {
	lines, newOffset, err := observe.ReadNewLines(path, fromOffset, maxBytes)
	if err != nil {
		return nil, fromOffset, err
	}
	var out []observe.Record
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var env envelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, env.Timestamp)
		if err != nil {
			continue // no reliable ordering without a timestamp — skip
		}
		if rec, ok := recordFromEnvelope(env, ts); ok {
			out = append(out, rec)
		}
	}
	return out, newOffset, nil
}

// recordFromEnvelope extracts the one Record (if any) a rollout line is
// worth: a genuine human ask, or a real git commit/push. Everything else —
// agent messages, reasoning, non-git tool calls, housekeeping lines — is
// not Turn-worthy, exactly as for memcode's own native session log.
func recordFromEnvelope(env envelope, ts time.Time) (observe.Record, bool) {
	switch env.Type {
	case "event_msg":
		var p eventMsgPayload
		if json.Unmarshal(env.Payload, &p) != nil || p.Type != "user_message" || strings.TrimSpace(p.Message) == "" {
			return observe.Record{}, false
		}
		return observe.Record{TS: ts, Kind: sessionlog.KindUserMessage, Text: observe.Clip(p.Message)}, true
	case "response_item":
		var p responseItemPayload
		if json.Unmarshal(env.Payload, &p) != nil || p.Type != "function_call" || p.Name != "exec_command" {
			return observe.Record{}, false
		}
		var args execCommandArgs
		if json.Unmarshal([]byte(p.Arguments), &args) != nil || !sessionlog.IsGitCommitPush(args.Cmd) {
			return observe.Record{}, false
		}
		return observe.Record{TS: ts, Kind: sessionlog.KindToolCall, Tool: "exec_command", Input: args.Cmd}, true
	default:
		return observe.Record{}, false
	}
}
