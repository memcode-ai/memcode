package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/scripts"
)

var scriptCmd = &cobra.Command{
	Use:   "script",
	Short: "Inspect saved reusable command sequences (read-only)",
	Long: `Scripts are reusable multi-step command sequences the agent saves after a proven run
(.memcode/scripts/<slug>.sh) — a recipe like "rebuild the cli" or "commit, push, deploy" replayed
by name instead of re-derived every time. Save/run/delete them from the agent via the ` + "`script`" + `
tool; these commands are for a human to inspect what's saved.`,
}

var scriptListCmd = &cobra.Command{
	Use:   "list",
	Short: "List saved scripts",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, _, err := config.Resolve(".")
		if err != nil {
			return err
		}
		all, err := scripts.List(root)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			fmt.Println("No saved scripts yet. The agent saves one with the `script` tool after a proven run.")
			return nil
		}
		for _, sc := range all {
			runs := "never run"
			if sc.RunCount > 0 {
				runs = fmt.Sprintf("run %d time(s), last %s", sc.RunCount, sc.LastRunAt.Format("2006-01-02 15:04"))
			}
			fmt.Printf("  %-24s %s  [%s]\n", sc.Slug, sc.Description, runs)
		}
		return nil
	},
}

var scriptShowCmd = &cobra.Command{
	Use:   "show <slug>",
	Short: "Show one saved script's full command",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, _, err := config.Resolve(".")
		if err != nil {
			return err
		}
		sc, ok := scripts.Get(root, args[0])
		if !ok {
			return fmt.Errorf("no saved script named %q", args[0])
		}
		fmt.Printf("%s\n%s\n\ncreated: %s\n", sc.Slug, sc.Description, sc.CreatedAt.Format("2006-01-02 15:04"))
		if !sc.LastRunAt.IsZero() {
			fmt.Printf("last run: %s (%d time(s))\n", sc.LastRunAt.Format("2006-01-02 15:04"), sc.RunCount)
		}
		fmt.Printf("\n%s\n", sc.Body)
		return nil
	},
}

func init() {
	scriptCmd.AddCommand(scriptListCmd, scriptShowCmd)
	rootCmd.AddCommand(scriptCmd)
}
