package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/buildinfo"
	"github.com/memcode-ai/memcode/internal/update"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the memcode version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("memcode %s\n", buildinfo.String())
		// Zero-network: reads only the cached daily update check.
		if n := update.NoticeFromCache(); n != "" {
			fmt.Println(n)
		}
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
	// Also wire `--version` on the root command.
	rootCmd.Version = buildinfo.Version
}
