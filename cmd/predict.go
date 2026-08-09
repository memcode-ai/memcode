package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/predict"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

var predictCmd = &cobra.Command{
	Use:   "next",
	Short: "Recommend the highest-value next move",
	Long: `Reads the repository's recent history and current uncommitted changes,
maps them onto the subsystem topology, and asks the model to recommend the single
highest-value next move. Secrets are never sent.

Requires MEMCODE_API_TOKEN (from the environment or a gitignored .env at the repo root; set MEMCODE_API_URL too).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		ev, err := predict.Gather(ctx, st, cfg.Root, "") // standalone CLI: no live session to exclude
		if err != nil {
			return err
		}

		// Deterministic "where" first — no model needed for this part.
		if len(ev.Subsystems) > 0 {
			fmt.Printf("where (from git): %s\n", strings.Join(ev.Subsystems, ", "))
		}
		if len(ev.DirtyFiles) > 0 {
			fmt.Printf("uncommitted: %d file(s)\n", len(ev.DirtyFiles))
		}
		fmt.Println()

		provider.LoadDotEnv()
		prov, err := provider.NewFromEnv()
		if err != nil {
			return err
		}
		user := predict.UserPrompt(ev)
		resp, err := llm.NewRunner(prov).Complete(ctx, llm.Predict, wire.Request{
			Mode:      "next",
			Messages:  []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: user}}}},
			MaxTokens: 1024, // the gateway resolves the model from purpose=predict
		})
		if err != nil {
			return err
		}
		fmt.Println(resp.Text())
		return nil
	},
}

func init() {
	rootCmd.AddCommand(predictCmd)
}
