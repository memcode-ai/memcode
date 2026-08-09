package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/stack"
)

// stack reports the detected languages + tech stack — deterministic, from the
// repo's manifests and source files (no model, no network).
var stackCmd = &cobra.Command{
	Use:   "stack",
	Short: "Detect the project's languages and tech stack",
	Long: `Scans the repository deterministically — language byte percentages plus the
frameworks, CLIs, databases, infra and CI parsed from manifests (go.mod,
package.json, …) and config files (Dockerfile, .github/workflows, …). No model,
no network. The same StackFacts feed /overview so it renders the stack from facts
instead of inferring it.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		root, _, err := config.Resolve(".")
		if err != nil {
			return err
		}
		facts, err := stack.LocalStackDetector{}.Detect(cmd.Context(), root)
		if err != nil {
			return err
		}
		fmt.Println(stack.Render(facts))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(stackCmd)
}
