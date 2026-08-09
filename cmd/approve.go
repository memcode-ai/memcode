package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
)

var approveCmd = &cobra.Command{
	Use:   "approve <command-pattern>",
	Short: "Remember an allow-rule so the agent stops re-prompting for it",
	Long: `Appends an allow-rule to .memcode/permissions so a matching command runs
without prompting in ask/auto mode. Patterns use '*' as a wildcard. That file is
plain text and yours to edit directly. Catastrophic commands (rm -rf, git reset
--hard, publish, cloud deploys) require --trusted to be remembered.

Examples:
  memcode approve "go test *"
  memcode approve "git push *" --trusted
  memcode approve list
  memcode approve revoke "go test *"`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		pattern := strings.Join(args, " ")
		trusted, _ := cmd.Flags().GetBool("trusted")

		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		if _, catastrophic := permissions.ClassifyBash(pattern); catastrophic && !trusted {
			return fmt.Errorf("%q is a catastrophic command; pass --trusted to remember it", pattern)
		}
		if err := permissions.Append(cfg.Root, pattern, trusted); err != nil {
			return err
		}
		note := ""
		if trusted {
			note = " [trusted]"
		}
		fmt.Printf("approved %q%s → %s\n", pattern, note, permissions.FilePath(cfg.Root))
		return nil
	},
}

var approveListCmd = &cobra.Command{
	Use:   "list",
	Short: "List remembered approvals",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		rules, err := permissions.Load(cfg.Root)
		if err != nil {
			return err
		}
		if len(rules) == 0 {
			fmt.Printf("No remembered approvals. Add one with `memcode approve \"<pattern>\"`, or edit %s.\n",
				permissions.FilePath(cfg.Root))
			return nil
		}
		for _, r := range rules {
			trust := ""
			if r.Trusted {
				trust = "  [trusted]"
			}
			fmt.Printf("  %s%s\n", r.Pattern, trust)
		}
		fmt.Printf("\n(%s — edit directly to change)\n", permissions.FilePath(cfg.Root))
		return nil
	},
}

var approveRevokeCmd = &cobra.Command{
	Use:   "revoke <pattern>",
	Short: "Remove a remembered approval (or just edit the permissions file)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		pattern := strings.Join(args, " ")
		ok, err := permissions.Remove(cfg.Root, pattern)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no approval matching %q", pattern)
		}
		fmt.Printf("revoked %q\n", pattern)
		return nil
	},
}

func init() {
	approveCmd.Flags().Bool("trusted", false, "also allow matching catastrophic commands")
	approveCmd.AddCommand(approveListCmd, approveRevokeCmd)
	rootCmd.AddCommand(approveCmd)
}
