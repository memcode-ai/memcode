package autonomy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type GeneratedIndex struct {
	Path, Hash, Purpose, SourceObjectiveID, SourceRunID, ParentRevision string
	BuildCommand, RunCommand, TestCommand                               []string
	Evaluations                                                         []EffectivenessEvaluation
	ActiveRevision                                                      string
}

func InitializeGeneratedWorkspace(home string) (string, error) {
	root := filepath.Join(home, "workspace", "generated")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); os.IsNotExist(err) {
		cmd := exec.Command("git", "init", "--quiet")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("initialize generated workspace: %v: %s", err, out)
		}
	}
	return root, nil
}
func CommitGenerated(root, message string) error {
	for _, args := range [][]string{{"add", "--all"}, {"-c", "user.name=Memcode Personal", "-c", "user.email=personal@localhost", "commit", "--quiet", "--allow-empty", "-m", message}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
	}
	return nil
}
func RollbackGenerated(root, revision string) error {
	cmd := exec.Command("git", "reset", "--hard", revision)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rollback generated workspace: %v: %s", err, out)
	}
	return nil
}

type EvolutionChoice string

const (
	EvolutionContinue       EvolutionChoice = "continue"
	EvolutionChangeStrategy EvolutionChoice = "change_strategy"
	EvolutionReuse          EvolutionChoice = "reuse_artifact"
	EvolutionGenerate       EvolutionChoice = "generate_artifact"
	EvolutionImprove        EvolutionChoice = "improve_artifact"
	EvolutionRetire         EvolutionChoice = "retire_artifact"
	EvolutionEscalate       EvolutionChoice = "request_information_or_authority"
	EvolutionAbandon        EvolutionChoice = "abandon"
)

func ChooseEvolution(e EffectivenessEvaluation, hasCompatibleArtifact bool) EvolutionChoice {
	if e.Success && e.Progress >= 1 {
		return EvolutionContinue
	}
	if e.UserCorrections >= 2 {
		return EvolutionEscalate
	}
	if e.Errors >= 3 {
		return EvolutionChangeStrategy
	}
	if e.RepeatedSteps >= 2 || e.CapabilityGap != "" {
		if hasCompatibleArtifact {
			return EvolutionImprove
		}
		return EvolutionGenerate
	}
	if e.Cost > 0 && hasCompatibleArtifact {
		return EvolutionReuse
	}
	return EvolutionContinue
}
