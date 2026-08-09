package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/browserrender"
)

// capabilitiesCmd lists optional, NOT-bundled local capabilities and their status.
// Today: browser-render (headless Chrome) for JavaScript-rendered pages that raw
// fetch and web_fetch can't read.
var capabilitiesCmd = &cobra.Command{
	Use:     "capabilities",
	Aliases: []string{"caps"},
	Short:   "List optional local capabilities and their status",
	Long: `Optional capabilities are heavy tools memcode does NOT bundle into its static
binary — you enable them locally when you actually need them.

  browser-render — render JavaScript-heavy pages with a local headless Chrome, used
                   only as the last fetch tier (after raw GET and server-side
                   web_fetch). Rendering executes the page's JavaScript locally, so
                   the agent asks for consent before using it. Use only for pages you
                   trust.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprintln(cmd.OutOrStdout(), "Optional local capabilities:")
		if path, ok := browserrender.Find(); ok {
			fmt.Fprintf(cmd.OutOrStdout(), "  ● browser-render  available — using %s\n", path)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "  ○ browser-render  not available — no Chrome/Chromium found")
			fmt.Fprintln(cmd.OutOrStdout(), "      install Google Chrome or Chromium to enable it (auto-download installer coming).")
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(capabilitiesCmd)
}
