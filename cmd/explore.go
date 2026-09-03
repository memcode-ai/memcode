package cmd

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/explore"
	"github.com/memcode-ai/memcode/internal/policy"
	"github.com/memcode-ai/memcode/internal/provider"
)

var exploreCmd = &cobra.Command{
	Use:   "explore <question>",
	Short: "Investigate the repo with many parallel read-only agents, then synthesize",
	Long: `Fans out a read-only "reader" sub-agent over each subsystem — every one
limited to read_file/ripgrep/git_diff, so they run concurrently with no risk of
clobbering the tree — then synthesizes their findings into one grounded answer.

This is the parallel half of memcode's "many readers, one serialized writer"
model. Use it to understand the codebase ("how does auth work?", "where is X
configured?") without editing anything.

Requires MEMCODE_API_TOKEN (from the environment or a gitignored .env at the repo root; set MEMCODE_API_URL too).`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		question := strings.Join(args, " ")

		st, cfg, _, runner, err := openModelProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		model := provider.EffectiveModel(cfg.Models.Coder)

		// Explore's model and its concurrency are both agent.explore policy.
		pol := sessionPolicy(cfg.Root, model).Resolve(policy.AgentExplore)
		if m := pol.Model("model"); m != "" {
			model = m
		}
		return explore.Run(ctx, st, runner, cfg.Root, model, question, pol.Int("concurrency"), userOut())
	},
}

func init() {
	rootCmd.AddCommand(exploreCmd)
}
