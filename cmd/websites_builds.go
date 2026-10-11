package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/websites"
)

// websites builds — a site's build history, newest first.
var websitesBuildsCmd = &cobra.Command{
	Use:   "builds <slug>",
	Short: "List a website's build history",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		site, err := c.Resolve(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		builds, err := c.ListBuilds(cmd.Context(), site.ID)
		if err != nil {
			return err
		}

		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			b, err := json.MarshalIndent(builds, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}

		if len(builds) == 0 {
			fmt.Println("No builds yet.")
			return nil
		}
		for _, b := range builds {
			line := fmt.Sprintf("%-10s %-8s %-8s", b.ID[:min(10, len(b.ID))], b.Mode, b.Status)
			if b.Error != "" {
				line += "  error: " + b.Error
			} else if b.Summary != "" {
				line += "  " + b.Summary
			}
			fmt.Println(line)
		}
		return nil
	},
}

func init() {
	websitesBuildsCmd.Flags().Bool("json", false, "output the build history as JSON")
	websitesCmd.AddCommand(websitesBuildsCmd)
}
