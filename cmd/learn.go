package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/acceptance"
	"github.com/memcode-ai/memcode/internal/learn"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
)

var learnCmd = &cobra.Command{
	Use:   "learn",
	Short: "Reconcile sources + evidence into adjudicated claims (current / stale / conflicted)",
	Long: `Extracts candidate claims from instruction/doc sources, folds in
deterministic facts (package manager, language, build/test commands, CGO), and
adjudicates each claim: corroborated → current, contradicts evidence →
conflicted, source lagged the code → stale. Replaces the stored claim set.

Inspect results with "memcode claims list" / "memcode claims conflicts".
Requires MEMCODE_API_TOKEN (from the environment or a gitignored .env at the repo root; set MEMCODE_API_URL too; secrets are redacted before any model call).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		// Opportunistically reconcile whether recent agent work survived (git).
		if out, _ := acceptance.Reconcile(ctx, st, cfg.Root); len(out) > 0 {
			fmt.Printf("reconciled %d agent session(s) against git.\n", len(out))
		}

		provider.LoadDotEnv(cfg.Root)
		prov, err := provider.NewFromEnv()
		if err != nil {
			return err
		}
		fmt.Println("reconciling sources + evidence into claims…")
		sum, err := learn.Run(ctx, st, llm.NewRunner(prov), cfg.Root)
		if err != nil {
			return err
		}
		fmt.Printf("learned %d claim(s): %d current, %d conflicted, %d stale, %d candidate\n",
			sum.Total, sum.Current, sum.Conflicted, sum.Stale, sum.Candidate)
		fmt.Println("see `memcode claims list` / `memcode claims conflicts`.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(learnCmd)
}
