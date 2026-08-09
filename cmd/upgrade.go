package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/buildinfo"
	"github.com/memcode-ai/memcode/internal/update"
)

// upgradeCmd self-updates the binary from GitHub Releases (the same artifacts
// the curl|sh installer ships). Checksum verification is mandatory — a corrupt
// download aborts before the running binary is touched.
var upgradeCmd = &cobra.Command{
	Use: "upgrade",
	// "update" is what half of users type first (apt/brew muscle memory) —
	// same command, no separate help entry.
	Aliases: []string{"update"},
	Short:   "Update memcode to the latest release",
	Long: `Checks GitHub Releases for a newer version, downloads the build for this
platform, verifies its checksum, and atomically replaces this binary.

The interactive session also self-updates once a day on startup (staged in the
background; the next launch runs it). Set MEMCODE_AUTO_UPDATE=off to keep
updates manual.

Use --check to only report whether an update exists.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if check, _ := cmd.Flags().GetBool("check"); check {
			latest, err := update.LatestVersion(ctx)
			if err != nil {
				return err
			}
			current := buildinfo.Compact()
			if update.IsNewer(current, latest) {
				fmt.Printf("update available: %s (you have %s) — run `memcode upgrade`\n", latest, current)
			} else {
				fmt.Printf("memcode %s is up to date (latest is %s)\n", current, latest)
			}
			return nil
		}
		_, err := update.Upgrade(ctx, cmd.OutOrStdout())
		return err
	},
}

func init() {
	upgradeCmd.Flags().Bool("check", false, "only check for a newer release, don't install")
	rootCmd.AddCommand(upgradeCmd)
}
