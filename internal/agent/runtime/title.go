package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/wire"
)

// A short, generated chat title for a session — like ChatGPT/Claude do — so a
// GUI sidebar (and `session recent`) shows a real name instead of a raw first
// message (which could be a giant paste). Generated ONCE per session via the
// cheap classify lane, persisted to a sidecar next to the transcript. The CLI
// owns it so it's not desktop-only.

const titleFile = "title"
const titleTimeout = 25 * time.Second

func titlePath(root, id string) string {
	return filepath.Join(root, ".memcode", "sessions", id, titleFile)
}

// TitleFor returns the saved chat title for a session, or "" if none was
// generated yet. Read by the machine-readable `session recent --json`.
func TitleFor(root, id string) string {
	b, err := os.ReadFile(titlePath(root, id))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// recordTitleTool forces the title as schema-constrained tool_use input (same
// forced-tool trick the risk/authorize classifiers use).
var recordTitleTool = wire.ToolDef{
	Name:        "record_title",
	Description: "Record a short title for this conversation. Call exactly once.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{"type": "string", "description": "3 to 6 words, Title Case, no quotes or trailing punctuation"},
		},
		"required": []string{"title"},
	},
}

// maybeGenerateTitle kicks off title generation at most once per session process,
// off the engine goroutine (best-effort — a failure just leaves the session
// untitled). Called at a turn boundary once there's real conversation to name.
func (s *Session) maybeGenerateTitle() {
	if s.sessionID == "" {
		return
	}
	s.titleOnce.Do(func() { go s.generateTitle() })
}

func (s *Session) generateTitle() {
	// Already named (e.g. a resumed session) — nothing to do.
	if TitleFor(s.root, s.sessionID) != "" {
		return
	}
	// The conversation so far, oldest first, as naming context (same source the
	// steer/anchor judges read; race-free tail of the append-only session log).
	hist := s.recentHistorySlice(6, 400, 2000)
	if strings.TrimSpace(hist) == "" {
		return
	}

	ctx, cancel := context.WithTimeout(s.bgCtx, titleTimeout)
	defer cancel()
	prompt := "Below is the start of a coding conversation between a user and an agent. " +
		"Write a short, specific title (3 to 6 words, Title Case) that names the task. " +
		"No quotes, no trailing punctuation.\n\n" + hist

	var out struct {
		Title string `json:"title"`
	}
	if err := s.classifyToolCall(ctx, "title", recordTitleTool, prompt, titleTimeout, &out); err != nil {
		return
	}
	title := cleanTitle(out.Title)
	if title == "" {
		return
	}
	p := titlePath(s.root, s.sessionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = atomicfile.WriteFile(p, []byte(title+"\n"), 0o644)
}

// cleanTitle trims quotes/whitespace and clamps length so a runaway model reply
// never becomes a giant sidebar entry.
func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'`")
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = s[:i]
	}
	return clip(s, 80)
}
