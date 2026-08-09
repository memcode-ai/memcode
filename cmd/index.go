package cmd

import (
	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/sources"
	"github.com/memcode-ai/memcode/internal/structure"
)

var indexCmd = &cobra.Command{
	Use:   "index",
	Short: "Refresh the deterministic model of the codebase",
	Long: `Re-scans the project and updates the structural state (subsystems,
dependencies, ownership, hotspots). Run after the topology changes; "memcode
init" does the same on first setup.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		res, err := structure.Scan(ctx, st, cfg.Root)
		if err != nil {
			return err
		}
		srcs, _ := sources.Discover(ctx, cfg.Root)
		_ = sources.Persist(ctx, st, srcs)
		printTopology(res)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(indexCmd)
}
