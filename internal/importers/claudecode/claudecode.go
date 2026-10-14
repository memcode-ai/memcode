// Package claudecode is an observe.Adapter over Claude Code's own local
// session transcripts (~/.claude/projects/<escaped-cwd>/*.jsonl — or
// $CLAUDE_CONFIG_DIR if set, the officially documented override). The
// format is NOT versioned or documented by the vendor; everything this file
// assumes was confirmed against a live sample at implementation time (see
// the field comments below) and parsing is defensive by construction — a
// line, or a line's shape, this package doesn't recognize is skipped, never
// fatal.
//
// Deliberately narrow in what it extracts, for two reasons at once: cost
// (see package observe's doc comment — no raw transcript text ever reaches
// disk, let alone a model) and fidelity to memcode's OWN doctrine for what's
// "meaningful" (sessionlog.KindToolCall's doc comment: real actions, not
// internal reads). A human's own ask, a real `git commit`/`push`, and a
// Write tool call whose full resulting content is recoverable (an Edit's
// is NOT — it carries only old/new fragments, not the whole file) are the
// only things that become a Record.
package claudecode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/edit"
	"github.com/memcode-ai/memcode/internal/observe"
	"github.com/memcode-ai/memcode/internal/sessionlog"
)

// Source is this adapter's provenance tag (focus.Turn.Source and the
// .memcode/observe/<source>/ directory name).
const Source = "claude-code"

// Importer is the zero-value observe.Adapter implementation — stateless,
// every method a pure function of its arguments.
type Importer struct{}

func (Importer) Source() string { return Source }

func (Importer) Discover(repoRoots []string) []string { return Discover(repoRoots) }

func (Importer) Import(path string, fromOffset, maxBytes int64) ([]observe.Record, int64, error) {
	return Import(path, fromOffset, maxBytes)
}

// baseDir returns Claude Code's config directory: $CLAUDE_CONFIG_DIR (the
// officially documented override, code.claude.com/docs/en/env-vars) if set,
// else ~/.claude.
func baseDir() (string, bool) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".claude"), true
}

// escapeCWD mirrors Claude Code's own ~/.claude/projects/<escaped-cwd>/
// naming — confirmed against a live sample: every path separator becomes a
// hyphen (e.g. "/Users/t/repo" -> "-Users-t-repo").
func escapeCWD(path string) string {
	return strings.ReplaceAll(path, string(filepath.Separator), "-")
}

// maxAge bounds which transcripts Discover even looks at: a session last
// touched more than this long ago is stale work, not "where you left off" —
// ingesting it would cost real parse time for zero orientation value. The
// SAME 7-day cutoff Sync's watermark-free first run and every subsequent
// run both respect, so total ingestion stays bounded by RECENCY, not just
// by per-call byte/file caps — "stay efficient, look at recent things,"
// never "ingest everything, eventually."
const maxAge = 7 * 24 * time.Hour

// Discover locates every Claude Code session transcript for the given repo
// roots (see gitlog.Roots — the project(s) under this workspace Claude Code
// may have worked in) that was modified within the last maxAge. Missing/
// inaccessible directories are simply absent from the result, never an
// error — most roots will have no Claude Code history at all.
func Discover(repoRoots []string) []string {
	base, ok := baseDir()
	if !ok {
		return nil
	}
	cutoff := time.Now().Add(-maxAge)
	var files []string
	for _, root := range repoRoots {
		dir := filepath.Join(base, "projects", escapeCWD(root))
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().Before(cutoff) {
				continue // stale (or unreadable) — not "where you left off"
			}
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files
}

// entry is the subset of one transcript line's shape this importer reads.
// Housekeeping lines (type "mode", "attachment", "file-history-snapshot",
// …) carry no "message" field at all and are skipped for free by the
// Message==nil check below — no type allowlist to maintain.
type entry struct {
	Timestamp string   `json:"timestamp"`
	Message   *message `json:"message"`
}

type message struct {
	Role string `json:"role"`
	// Content is a plain JSON string for a genuine human ask, or an array of
	// contentBlock for everything else (assistant turns, and a tool_result
	// handed back as a "user"-role turn) — RawMessage so either shape parses
	// without a custom UnmarshalJSON.
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type  string          `json:"type"` // "text" | "thinking" | "tool_use" | "tool_result"
	Name  string          `json:"name"` // tool_use: the tool name ("Bash", "Write", "Edit", …)
	Input json.RawMessage `json:"input"`
}

type bashInput struct {
	Command string `json:"command"`
}

type writeInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// Import parses the new lines appended to path since fromOffset (via
// observe.ReadNewLines, which bounds how much is read and guarantees a line
// is never split across two calls), up to maxBytes of new content, and
// returns the Records extracted plus the offset to resume from next time. A
// line that fails to parse is skipped, never fatal to the rest of the file.
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
		var e entry
		if json.Unmarshal(line, &e) != nil || e.Message == nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			continue // no reliable ordering without a timestamp — skip
		}
		out = append(out, recordsFromMessage(*e.Message, ts)...)
	}
	return out, newOffset, nil
}

// recordsFromMessage extracts every Turn-worthy Record from one message —
// zero, one, or (an assistant turn with several tool calls) more than one.
func recordsFromMessage(m message, ts time.Time) []observe.Record {
	if m.Role == "user" {
		var ask string
		if json.Unmarshal(m.Content, &ask) != nil || strings.TrimSpace(ask) == "" {
			return nil // not a plain-string human ask — e.g. a tool_result turn
		}
		return []observe.Record{{TS: ts, Kind: sessionlog.KindUserMessage, Text: observe.Clip(ask)}}
	}
	if m.Role != "assistant" {
		return nil
	}
	var blocks []contentBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil
	}
	var out []observe.Record
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue // text/thinking: not fed into strand detection even for memcode's OWN log
		}
		switch b.Name {
		case "Bash":
			var in bashInput
			if json.Unmarshal(b.Input, &in) == nil && sessionlog.IsGitCommitPush(in.Command) {
				out = append(out, observe.Record{TS: ts, Kind: sessionlog.KindToolCall, Tool: "Bash", Input: in.Command})
			}
		case "Write":
			var in writeInput
			if json.Unmarshal(b.Input, &in) == nil && in.FilePath != "" {
				out = append(out, observe.Record{
					TS: ts, Kind: sessionlog.KindToolCall, Tool: "Write",
					FileClaim: &observe.FileClaim{Path: in.FilePath, ClaimedHash: edit.HashBytes([]byte(in.Content)), ClaimedAt: ts},
				})
			}
			// Edit is deliberately NOT handled: old_string/new_string are
			// fragments, not the file's full resulting content, so no
			// ClaimedHash is recoverable from the transcript alone.
		}
	}
	return out
}
