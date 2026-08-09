package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/assemble"
)

var contextCmd = &cobra.Command{
	Use:   "context <path|query>",
	Short: "Compile a structured context pack for a file, directory or task",
	Long: `Assembles what an agent (or you) should know before touching something:
the subsystem it lives in, what it depends on, the objectives in play, recent
activity, and the files worth reading first — every item carrying the reason it
was included. Fully deterministic (topology + objectives + git + content search);
no model, no embeddings.

The same structured ContextPack the CLI renders is what the agent's "orient" step
will consume. Use --json to see it.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		asJSON, _ := cmd.Flags().GetBool("json")
		target := strings.Join(args, " ")

		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		pack, err := assemble.Context(ctx, st, cfg.Root, target)
		if err != nil {
			return err
		}

		if asJSON {
			b, err := json.MarshalIndent(pack, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}
		renderPack(pack)
		return nil
	},
}

func renderPack(p assemble.ContextPack) {
	fmt.Printf("context: %s", p.Target)
	if p.Subsystem != "" {
		fmt.Printf("   (subsystem: %s, %s)", p.Subsystem, p.Ecosystem)
	}
	fmt.Println()

	section("objectives in play", p.Objectives, func(it assemble.Item) string {
		return fmt.Sprintf("%s — %s", it.Label, it.Reason)
	})
	section("dependencies", p.Dependencies, func(it assemble.Item) string {
		return fmt.Sprintf("%s (%s)", it.Ref, it.Reason)
	})
	section("relevant files", p.RelevantFiles, func(it assemble.Item) string {
		return fmt.Sprintf("%-44s %s", it.Ref, it.Reason)
	})
	section("recent activity", p.RecentEvents, func(it assemble.Item) string {
		return fmt.Sprintf("%s  %s", it.Ref, it.Label)
	})
	section("constraints", p.Constraints, func(it assemble.Item) string {
		return fmt.Sprintf("%s — %s", it.Ref, it.Reason)
	})
	section("recommended reads", p.RecommendedReads, func(it assemble.Item) string {
		return fmt.Sprintf("%-44s %s", it.Ref, it.Reason)
	})

	if len(p.RankingReasons) > 0 {
		fmt.Printf("\nhow this was assembled:\n")
		for _, r := range p.RankingReasons {
			fmt.Printf("  · %s\n", r)
		}
	}
	flag := ""
	if p.TokenBudget.Truncated {
		flag = "  ⚠ over budget"
	}
	fmt.Printf("\nbudget: ~%d / %d tokens%s\n", p.TokenBudget.Estimated, p.TokenBudget.Limit, flag)
}

// section prints a titled list, skipping empty ones with an honest "(none)".
func section(title string, items []assemble.Item, fmtItem func(assemble.Item) string) {
	fmt.Printf("\n%s:\n", title)
	if len(items) == 0 {
		fmt.Println("  (none)")
		return
	}
	for _, it := range items {
		fmt.Printf("  %s\n", fmtItem(it))
	}
}

func init() {
	contextCmd.Flags().Bool("json", false, "output the raw ContextPack as JSON")
	rootCmd.AddCommand(contextCmd)
}
