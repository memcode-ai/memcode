package autonomy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/jobs"
)

type ExecutionEnvelope struct {
	Task, ExpectedOutput, CompletionCondition string
	Context                                   json.RawMessage
	Toolsets                                  []string
	Resources                                 []string
	Consequences                              []ConsequenceClass
	Deadline                                  string
	Budgets                                   jobs.ExecutionBudgets
	ParentRunID, SubgoalID                    string
	AllowDelegation                           bool
	DelegationDepth                           int
	// BrowserSession selects the worker's browser backend when Toolsets
	// includes "browser": BrowserExistingChrome (the default for Personal
	// Agent delegation — the user's own already-running, already-logged-in
	// Chrome, reached through the gateway-owned broker) or BrowserEphemeral
	// (a fresh, logged-out profile — explicit opt-down only). See
	// docs/design/personal-agents.md "Browser broker trust boundary".
	BrowserSession string
}

const (
	BrowserExistingChrome = "existing_chrome"
	BrowserEphemeral      = "ephemeral"
)

func ValidateDelegation(parent DelegationPolicy, e ExecutionEnvelope) error {
	if e.Task == "" || e.CompletionCondition == "" {
		return fmt.Errorf("worker task and completion condition are required")
	}
	if e.DelegationDepth > parent.MaxDelegationDepth {
		return fmt.Errorf("delegation depth exceeds policy")
	}
	// An empty parent.AllowedTools means "no restriction by name" — the same
	// convention Executive.allowedTools uses (restrictByName := len(...) > 0).
	// Treating empty as "allows nothing" here would make every delegate call
	// fail for the common case of a policy that doesn't bother naming tools.
	if len(parent.AllowedTools) > 0 && !subset(e.Toolsets, parent.AllowedTools) {
		return fmt.Errorf("worker tools expand parent authority")
	}
	if !classSubset(e.Consequences, parent.ConsequenceClasses) {
		return fmt.Errorf("worker consequences expand parent authority")
	}
	return nil
}
func PrepareRunDirectory(home, runID string, e ExecutionEnvelope) (string, error) {
	dir := filepath.Join(home, "runs", runID)
	if err := os.MkdirAll(filepath.Join(dir, "scratch"), 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "evidence"), 0o700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", err
	}
	if err := atomicfile.WriteFile(filepath.Join(dir, "envelope.json"), b, 0o600); err != nil {
		return "", err
	}
	return dir, nil
}
