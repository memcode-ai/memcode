package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/wire"
)

type Outcome string

const (
	OutcomeCompleted Outcome = "completed"
	OutcomeFailed    Outcome = "failed"
	OutcomeSuspended Outcome = "suspended"
)

type Suspension struct {
	Version                                       int `json:"version"`
	SessionID, InteractionID, ToolUseID, ToolName string
	ToolInput                                     json.RawMessage
	Assistant                                     wire.Message
	CreatedAt                                     time.Time
	Resolved                                      bool
}

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

func suspensionPath(root, sessionID, interactionID string) string {
	return filepath.Join(root, ".memcode", "sessions", sessionID, "continuations", interactionID+".json")
}

func SaveSuspension(root string, s Suspension) error {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	if err := ValidateSingletonSuspension(s.Assistant, s.ToolUseID); err != nil {
		return err
	}
	p := suspensionPath(root, s.SessionID, s.InteractionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(p, b, 0o600)
}

func LoadSuspension(root, sessionID, interactionID string) (Suspension, error) {
	b, err := os.ReadFile(suspensionPath(root, sessionID, interactionID))
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

func ResolveSuspension(root string, s Suspension, result wire.Block) ([]wire.Message, error) {
	if s.Resolved {
		return nil, fmt.Errorf("interaction %q is already resolved", s.InteractionID)
	}
	if result.Type != "tool_result" || result.ToolUseID != s.ToolUseID {
		return nil, fmt.Errorf("tool result id %q does not match suspended tool %q", result.ToolUseID, s.ToolUseID)
	}
	s.Resolved = true
	p := suspensionPath(root, s.SessionID, s.InteractionID)
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if err := atomicfile.WriteFile(p, b, 0o600); err != nil {
		return nil, err
	}
	return []wire.Message{s.Assistant, {Role: "user", Blocks: []wire.Block{result}}}, nil
}
