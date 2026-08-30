package personal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/jobs"
)

func TestDynamicDelegationNarrowingAndRunDirectory(t *testing.T) {
	parent := DelegationPolicy{AllowedTools: []string{"files", "shell"}, ConsequenceClasses: []ConsequenceClass{Observe, LocalMutation}, MaxDelegationDepth: 2}
	e := ExecutionEnvelope{Task: "Arbitrary objective-specific investigation", ExpectedOutput: "evidence", CompletionCondition: "evidence recorded", Toolsets: []string{"files"}, Consequences: []ConsequenceClass{Observe}, Budgets: jobs.ExecutionBudgets{MaxSeconds: 60}, DelegationDepth: 1}
	if err := ValidateDelegation(parent, e); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	dir, err := PrepareRunDirectory(home, "run-1", e)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"envelope.json", "scratch", "evidence"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Fatal(err)
		}
	}
	e.Toolsets = []string{"browser"}
	if err := ValidateDelegation(parent, e); err == nil {
		t.Fatal("expanded worker authority accepted")
	}
}
func TestDelegationHasNoRoleRequirement(t *testing.T) {
	p := DelegationPolicy{AllowedTools: []string{"files"}, ConsequenceClasses: []ConsequenceClass{Observe}, MaxDelegationDepth: 1}
	if err := ValidateDelegation(p, ExecutionEnvelope{Task: "Any dynamically described work", CompletionCondition: "done", Toolsets: []string{"files"}, Consequences: []ConsequenceClass{Observe}}); err != nil {
		t.Fatal(err)
	}
}
