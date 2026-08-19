package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/overview"
	"github.com/memcode-ai/memcode/internal/provider"
)

var overviewCmd = &cobra.Command{
	Use:   "overview",
	Short: "Synthesized current-state overview — what the project is and what's being worked on now",
	Long: `Produces a CANONICAL current overview by synthesizing fresh signals — recent
commits, active objectives, current claims, and recently-active subsystems — not
by rebuilding from old docs. Cached per commit (HEAD); regenerated when work moves
on, or with --refresh.

Requires MEMCODE_API_TOKEN (from the environment or a gitignored .env at the repo root; set MEMCODE_API_URL too).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		refresh, _ := cmd.Flags().GetBool("refresh")

		st, cfg, _, runner, err := openModelProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		model := provider.EffectiveModel(cfg.Models.Coder)

		var o overview.Overview
		if refresh {
			o, err = overview.Synthesize(ctx, st, runner, cfg.Root, model)
		} else {
			o, err = overview.Get(ctx, st, runner, cfg.Root, model)
		}
		if err != nil {
			return err
		}
		fmt.Println(o.Text)
		return nil
	},
}

func init() {
	overviewCmd.Flags().Bool("refresh", false, "re-synthesize even if a fresh cached overview exists")
	rootCmd.AddCommand(overviewCmd)
}
