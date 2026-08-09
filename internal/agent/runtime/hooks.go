package runtime

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/memcode-ai/memcode/internal/hooks"
	"github.com/memcode-ai/memcode/internal/wire"
)

// User hooks integration (see internal/hooks): pre_tool_use can VETO a tool
// call deterministically — a guard the prompt can't provide — and
// post_tool_use observes results. Wired here, around executeBatch, so the
// whole tool surface (main loop + plan loop) passes through one gate.

// hooksNow lazily loads the merged hook set for this session and surfaces any
// load warnings once. Cheap when no hooks.json exists.
func (s *Session) hooksNow() *hooks.Set {
	if s.hookSet == nil {
		s.hookSet = hooks.Load(s.root)
		for _, w := range s.hookSet.Warnings() {
			s.printf("  hooks: %s\n", w)
		}
	}
	// Re-stamp every use: the session id changes across /resume and /fork while
	// the loaded set is cached — hook commands must see the CURRENT id.
	s.hookSet.SetSessionID(s.sessionID)
	return s.hookSet
}

// executeBatchHooked wraps executeBatch with the user's pre/post_tool_use
// hooks. A blocked call never executes; its tool_result carries the hook's
// reason (IsError) so the model can adapt instead of stalling.
func (s *Session) executeBatchHooked(ctx context.Context, uses []wire.Block) []wire.Block {
	s.snapshotEdits(uses) // rewind pre-images — before ANY execution, hooks or not
	hs := s.hooksNow()
	if hs.Empty() {
		return s.executeBatch(ctx, uses)
	}

	allowed := make([]wire.Block, 0, len(uses))
	var vetoed []wire.Block
	for _, u := range uses {
		if reason, blocked := s.preToolVeto(ctx, hs, u); blocked {
			s.printf("  ⨯ %s blocked by hook: %s\n", u.Name, reason)
			vetoed = append(vetoed, wire.Block{
				Type: "tool_result", ToolUseID: u.ID, IsError: true,
				Content: "A user-configured pre_tool_use hook blocked this call: " + reason,
			})
			continue
		}
		allowed = append(allowed, u)
	}

	var results []wire.Block
	if len(allowed) > 0 {
		results = s.executeBatch(ctx, allowed)
	}

	for _, u := range allowed {
		payload := map[string]any{
			"event": hooks.PostToolUse, "tool": u.Name, "input": json.RawMessage(u.Input),
			"session_id": s.sessionID, "root": s.root,
			"result": resultContentFor(results, u.ID), "is_error": resultIsError(results, u.ID),
		}
		for _, r := range hs.Run(ctx, hooks.PostToolUse, u.Name, payload) {
			if r.Message != "" {
				s.printf("  hooks: %s\n", r.Message)
			}
		}
	}
	return append(results, vetoed...)
}

// preToolVeto runs the pre_tool_use hooks for one call. First block wins.
func (s *Session) preToolVeto(ctx context.Context, hs *hooks.Set, u wire.Block) (string, bool) {
	payload := map[string]any{
		"event": hooks.PreToolUse, "tool": u.Name, "input": json.RawMessage(u.Input),
		"session_id": s.sessionID, "root": s.root,
	}
	for _, r := range hs.Run(ctx, hooks.PreToolUse, u.Name, payload) {
		if r.Block {
			return r.Message, true
		}
		if r.Message != "" {
			s.printf("  hooks: %s\n", r.Message)
		}
	}
	return "", false
}

// runSessionHooks fires a session_start/session_end event and returns any
// stdout the hooks produced (session_start context injection).
func (s *Session) runSessionHooks(ctx context.Context, event string) string {
	hs := s.hooksNow()
	if hs.Empty() {
		return ""
	}
	payload := map[string]any{"event": event, "session_id": s.sessionID, "root": s.root}
	var parts []string
	for _, r := range hs.Run(ctx, event, "", payload) {
		if r.Message != "" {
			s.printf("  hooks: %s\n", r.Message)
		}
		if out := strings.TrimSpace(r.Stdout); out != "" {
			parts = append(parts, out)
		}
	}
	return strings.Join(parts, "\n\n")
}

func resultContentFor(results []wire.Block, toolUseID string) string {
	for _, r := range results {
		if r.ToolUseID == toolUseID {
			if r.Content != "" {
				return truncateForHook(r.Content)
			}
			var sb strings.Builder
			for _, cb := range r.ContentBlocks {
				if cb.Type == "text" {
					sb.WriteString(cb.Text)
				}
			}
			return truncateForHook(sb.String())
		}
	}
	return ""
}

func resultIsError(results []wire.Block, toolUseID string) bool {
	for _, r := range results {
		if r.ToolUseID == toolUseID {
			return r.IsError
		}
	}
	return false
}

func truncateForHook(s string) string {
	const limit = 16 << 10
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n… (truncated)"
}
