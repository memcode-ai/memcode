package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/overview"
)

// arch prints the project's architecture/flow diagrams — extracted verbatim from
// the repo's own docs (ARCH.md / README). Never synthesized: present when the docs
// document it, absent otherwise. Deterministic, no model, no network.
var archCmd = &cobra.Command{
	Use:   "arch",
	Short: "Show the architecture/flow diagrams from the repo's docs",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, _, err := config.Resolve(".")
		if err != nil {
			return err
		}
		fmt.Println(overview.Arch(root))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(archCmd)
}
