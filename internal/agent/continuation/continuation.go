// Package continuation is the ONE durable suspend/resume mechanism for an agent
// turn that stops mid-flight to wait for a human.
//
// It exists as its own package because two very different callers need it and
// neither should depend on the other: an interactive session (which already
// holds the conversation in memory and only needs the missing pair of messages
// back) and an unattended executive (which keeps no transcript at all — it
// rebuilds context from durable state each wake, so the continuation must carry
// the conversation itself). Before this package there were three partial
// designs: a typed-but-unused one in internal/agent/runtime, a set of
// declared-but-never-written fields on jobs.Job, and a hand-rolled map[string]any
// in the personal executive that was the only one actually running. Keep it one.
//
// The invariant that makes resume exact: a suspending tool must be the SOLE
// tool use in its assistant response (ValidateSingletonSuspension). Otherwise a
// sibling tool call in the same batch would be silently dropped on resume, or
// re-executed — both wrong. Save refuses to write a suspension that violates it,
// so the error surfaces at suspend time rather than as corruption at resume.
package continuation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Suspension is the exact continuation of a turn paused on a human answer.
type Suspension struct {
	Version       int             `json:"version"`
	SessionID     string          `json:"session_id,omitempty"`
	RunID         string          `json:"run_id,omitempty"`
	InteractionID string          `json:"interaction_id"`
	ToolUseID     string          `json:"tool_use_id"`
	ToolName      string          `json:"tool_name,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
	// Assistant is the response carrying the suspending tool use.
	Assistant wire.Message `json:"assistant"`
	// Messages is the full transcript up to and including Assistant. Set it
	// when the caller keeps no transcript of its own; Resolve then hands back
	// the whole conversation plus the answer. Leave it empty when the caller
	// still holds the prior turns — Resolve then returns only the two messages
	// to append, so the transcript is never duplicated.
	Messages  []wire.Message `json:"messages,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	Resolved  bool           `json:"resolved"`
}

// SessionDir is where an interactive session's continuations live.
func SessionDir(root, sessionID string) string {
	return filepath.Join(root, ".memcode", "sessions", sessionID, "continuations")
}

func path(dir, interactionID string) string {
	return filepath.Join(dir, interactionID+".json")
}

// ValidateSingletonSuspension enforces that the suspending tool is the only
// tool use in its assistant response — see the package comment.
func ValidateSingletonSuspension(msg wire.Message, toolUseID string) error {
	var tools int
	for _, b := range msg.Blocks {
		if b.Type == "tool_use" {
			tools++
			if b.ID != toolUseID && toolUseID != "" {
				return fmt.Errorf("suspending tool %q does not match assistant tool use %q", toolUseID, b.ID)
			}
		}
	}
	if tools != 1 {
		return fmt.Errorf("a suspending action must be the only tool use in its assistant response; got %d tool uses", tools)
	}
	return nil
}

// Save writes the continuation atomically. A crash mid-write must not be able
// to leave a truncated file — that would strand the interaction with no way to
// resume it.
func Save(dir string, s Suspension) error {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	if err := ValidateSingletonSuspension(s.Assistant, s.ToolUseID); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path(dir, s.InteractionID), b, 0o600)
}

// Load reads an unresolved continuation. An already-resolved one is an error,
// not an empty result: resuming it twice would re-execute real side effects.
func Load(dir, interactionID string) (Suspension, error) {
	b, err := os.ReadFile(path(dir, interactionID))
	if err != nil {
		return Suspension{}, err
	}
	var s Suspension
	if err := json.Unmarshal(b, &s); err != nil {
		return Suspension{}, err
	}
	if s.Resolved {
		return Suspension{}, fmt.Errorf("interaction %q is already resolved", interactionID)
	}
	return s, nil
}

// ResumeMessages builds the messages to resume with — the full transcript plus
// the answer when Messages was recorded, or just the assistant/answer pair when
// the caller holds the transcript itself — WITHOUT marking the continuation
// resolved.
//
// Separated from Resolve because the two callers differ on when it is safe to
// mark: an interactive session has the human right there and can simply ask
// again, so it marks immediately; an unattended executive must keep the
// interaction retryable until the resumed run actually reaches a terminal
// state, or a transient model error would strand the answer with no way to
// re-give it.
//
// The result block must match the suspended tool_use_id — a mismatched pairing
// is how a resume silently answers the wrong question.
func (s Suspension) ResumeMessages(result wire.Block) ([]wire.Message, error) {
	if s.Resolved {
		return nil, fmt.Errorf("interaction %q is already resolved", s.InteractionID)
	}
	if result.Type != "tool_result" || result.ToolUseID != s.ToolUseID {
		return nil, fmt.Errorf("tool result id %q does not match suspended tool %q", result.ToolUseID, s.ToolUseID)
	}
	answer := wire.Message{Role: "user", Blocks: []wire.Block{result}}
	if len(s.Messages) > 0 {
		return append(append([]wire.Message{}, s.Messages...), answer), nil
	}
	return []wire.Message{s.Assistant, answer}, nil
}

// Resolve builds the resume messages and marks the continuation resolved in one
// step, for a caller that can afford to lose the retry (see ResumeMessages).
func Resolve(dir string, s Suspension, result wire.Block) ([]wire.Message, error) {
	out, err := s.ResumeMessages(result)
	if err != nil {
		return nil, err
	}
	if err := MarkResolved(dir, s.InteractionID); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkResolved records that a continuation has been consumed without producing
// resume messages — for a caller that resolved the interaction by another route
// (cancelled, or a resumed run that itself re-suspended on a new question) and
// must not leave the old file loadable.
func MarkResolved(dir, interactionID string) error {
	s, err := Load(dir, interactionID)
	if err != nil {
		return err
	}
	s.Resolved = true
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path(dir, interactionID), b, 0o600)
}
