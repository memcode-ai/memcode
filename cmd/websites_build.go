package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/websites"
)

// websites build — queue a hosted build for an existing site: from a fresh
// prompt, or a no-prompt rebuild of its current state. Internal-preview
// gated (canSeeTestPlans): for other accounts it fails with ErrBuildsGated
// and a non-zero exit, so scripts (and --json consumers) can tell.
var websitesBuildCmd = &cobra.Command{
	Use:   "build <slug>",
	Short: "Queue a hosted build for a website (internal preview)",
	Long: `Queue a hosted build.

  memcode websites build cafe --prompt "add a contact form"
  memcode websites build cafe                 # rebuild, no new prompt

Hosted building is limited to internal accounts right now; other accounts
get an error saying so.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		site, err := c.Resolve(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		prompt, _ := cmd.Flags().GetString("prompt")
		model, _ := cmd.Flags().GetString("model")
		asJSON, _ := cmd.Flags().GetBool("json")

		build, err := c.Build(cmd.Context(), site.ID, prompt, model)
		if err != nil {
			return err
		}

		if asJSON {
			b, err := json.MarshalIndent(build, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}
		fmt.Printf("Build queued (%s), status %s\n", build.ID, build.Status)
		fmt.Printf("Check progress with `memcode websites builds %s`\n", args[0])
		return nil
	},
}

func init() {
	websitesBuildCmd.Flags().String("prompt", "", "describe the change; omit for a plain rebuild")
	websitesBuildCmd.Flags().String("model", "", "chat model id for this build (default: the site's pinned model)")
	websitesBuildCmd.Flags().Bool("json", false, "output the queued build as JSON")
	websitesCmd.AddCommand(websitesBuildCmd)
}
