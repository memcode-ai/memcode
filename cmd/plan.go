package cmd

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
)

var planCmd = &cobra.Command{
	Use:   "plan <task>",
	Short: "Research the codebase and propose a detailed plan (makes no changes)",
	Long: `Plan mode decides WHAT to do. memcode researches the repository read-only
with the reasoning model and writes a detailed, step-by-step proposal — it makes
NO edits or commands. Use the interactive session (the /plan command) to iterate
on a plan and then execute it; this one-shot form just prints the proposal.

Requires MEMCODE_API_TOKEN (from the environment or a gitignored .env at the repo root; set MEMCODE_API_URL too).`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		provider.LoadDotEnv()
		prov, err := provider.NewFromEnv()
		if err != nil {
			return err
		}
		planner := provider.EffectiveModel(cfg.Models.Planner)
		research := provider.EffectiveModel(cfg.Models.Coder)
		// Base = research model so the read-only loop + explorers run cheap; the
		// final plan is synthesized on the planner (reasoning) model.
		sess := runtime.New(st, llm.NewRunner(prov), cfg.Root, research, permissions.ModeAsk, userOut())
		sess.SetPlannerModel(planner)
		sess.SetPlanResearchModel(research)

		_, err = sess.RunPlan(ctx, strings.Join(args, " "))
		return err
	},
}

func init() {
	rootCmd.AddCommand(planCmd)
}
