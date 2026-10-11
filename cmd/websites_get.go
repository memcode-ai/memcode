package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/websites"
)

// websites get — one site's full detail (model, preview/publish state),
// resolved by slug the same way every other websites subcommand does.
var websitesGetCmd = &cobra.Command{
	Use:   "get <slug>",
	Short: "Show one website's details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		site, err := c.GetBySlug(cmd.Context(), args[0])
		if err != nil {
			return err
		}

		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON {
			b, err := json.MarshalIndent(site, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}

		fmt.Printf("%s (%s)\n", site.Name, site.Slug)
		status := site.Status
		if status == "" {
			status = "draft"
		}
		fmt.Printf("  status:  %s\n", status)
		if site.Model != "" {
			fmt.Printf("  model:   %s\n", site.Model)
		}
		if site.LiveURL != "" {
			fmt.Printf("  url:     %s\n", site.LiveURL)
		}
		if site.PublishedAt != "" {
			fmt.Printf("  published: %s\n", site.PublishedAt)
		}
		fmt.Printf("  preview: %v\n", site.HasPreview)
		return nil
	},
}

func init() {
	websitesGetCmd.Flags().Bool("json", false, "output the site as JSON")
	websitesCmd.AddCommand(websitesGetCmd)
}
