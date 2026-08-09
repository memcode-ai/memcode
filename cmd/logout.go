package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/authflow"
)

// logoutCmd removes the MEMCODE_API_TOKEN (and MEMCODE_API_URL) from the global
// env file, effectively signing the CLI out. The token remains valid server-side
// until revoked from the web app (a /api/cli/revoke endpoint is a follow-up).
var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Sign out of memcode",
	RunE: func(cmd *cobra.Command, args []string) error {
		removed, err := authflow.StripGlobalEnvToken()
		if err != nil {
			return err
		}
		if !removed {
			fmt.Println("  Already logged out (no token in config).")
			return nil
		}
		fmt.Println("  ✓ Logged out. Token removed from global config.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(logoutCmd)
}
