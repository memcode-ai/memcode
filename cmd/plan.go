package cmd

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/config"
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
		st, cfg, _, runner, err := openModelProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		// The session's model comes from the pin chain like everywhere else;
		// delegated work (the read-only research loop's explorers) rides the
		// delegated pin when one is configured.
		pin, win := config.ResolvePin(cfg, "")
		sess := runtime.New(st, runner, cfg.Root, pin, permissions.ModeAsk, userOut())
		sess.SetPin(pin, win)
		if dp, dw := config.ResolveDelegatedPin(cfg, "", pin, win); dp != pin {
			sess.SetDelegatedPin(dp, dw)
		}

		_, err = sess.RunPlan(ctx, strings.Join(args, " "))
		return err
	},
}

func init() {
	rootCmd.AddCommand(planCmd)
}
