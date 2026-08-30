package cmd

import (
	"fmt"
	"time"

	"github.com/memcode-ai/memcode/internal/personal"
	"github.com/spf13/cobra"
)

var personalResourcesCmd = &cobra.Command{Use: "resources", Short: "Manage resource grants"}

var personalResourcesAddCmd = &cobra.Command{
	Use: "add <agent> [type] <locator>", Args: cobra.RangeArgs(2, 3),
	Short: "Grant a resource (filesystem path, mcp tool, command, channel)",
	Long: `Grant a resource to a Personal Agent.

For a filesystem path, type is optional and inferred — a bare path is enough:

  memcode personal resources add jobhunt ~/resume.md

Non-filesystem grants (mcp, command, channel) need the type spelled out:

  memcode personal resources add jobhunt mcp gmail`,
	RunE: func(cmd *cobra.Command, args []string) error {
		st, home, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		mode, _ := cmd.Flags().GetString("mode")
		// Two positional args (agent, locator): type is inferred as filesystem
		// when the locator actually resolves to a real path on disk — that's
		// the common case (grant a file/dir), and it fails loudly rather than
		// guessing when it doesn't resolve. Three args names the type
		// explicitly, required for mcp/command/channel (nothing on disk to
		// resolve against).
		var rtype, locator string
		if len(args) == 2 {
			rtype, locator = "filesystem", args[1]
		} else {
			rtype, locator = args[1], args[2]
		}
		if rtype == "filesystem" {
			canon, err := personal.CanonicalFilesystemGrant(locator)
			if err != nil {
				return fmt.Errorf("cannot grant filesystem path: %w", err)
			}
			locator = canon
		}
		id := fmt.Sprintf("res-%s-%d", rtype, time.Now().UnixNano())
		if err := st.InsertResource(cmd.Context(), personal.Resource{
			ID: id, ObjectiveID: "primary", Type: rtype, Locator: locator,
			AccessMode: mode, AuthorizationSource: "user-cli", Status: "active",
		}); err != nil {
			return err
		}
		_ = personal.WriteConfigMirror(cmd.Context(), home, st)
		fmt.Fprintf(cmd.OutOrStdout(), "Granted %s %s (%s) to %s.\n", rtype, locator, mode, args[0])
		return nil
	},
}

var personalResourcesListCmd = &cobra.Command{
	Use: "list <agent>", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		st, _, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		res, err := st.ListResources(cmd.Context(), "primary")
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(res) == 0 {
			fmt.Fprintln(out, "no resource grants — the agent can only use its own home")
			return nil
		}
		for _, r := range res {
			fmt.Fprintf(out, "- %s: %s %s (%s) [%s]\n", r.ID, r.Type, r.Locator, r.AccessMode, r.Status)
		}
		return nil
	},
}

var personalResourcesRevokeCmd = &cobra.Command{
	Use: "revoke <agent> <resource-id>", Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		st, home, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.SetResourceStatus(cmd.Context(), args[1], "revoked"); err != nil {
			return err
		}
		_ = personal.WriteConfigMirror(cmd.Context(), home, st)
		fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s on %s (effective at the next dispatch).\n", args[1], args[0])
		return nil
	},
}

func init() {
	personalResourcesAddCmd.Flags().String("mode", "read", "access mode: read, write, or admin")
	personalResourcesCmd.AddCommand(personalResourcesAddCmd, personalResourcesListCmd, personalResourcesRevokeCmd)
}
