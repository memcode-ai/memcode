package cmd

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/websites"
)

// websites create — makes a new site row (name and/or a build prompt), then
// — when a prompt was given — tries to queue a build from it. Building is
// internal-preview gated (canSeeTestPlans): the site is created either way,
// and a gated build is reported as such, never as a failure of `create`.
var websitesCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a new website, optionally building it from a prompt",
	Long: `Create a new website.

  memcode websites create "Acme Coffee Roasters"
  memcode websites create --prompt "a one-page site for my coffee shop"

Creation always succeeds. If --prompt is given, a build is queued from it —
but hosted building is limited to internal accounts right now, so for most
accounts the site is created empty and the build is reported as unavailable.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		var name string
		if len(args) > 0 {
			name = args[0]
		}
		prompt, _ := cmd.Flags().GetString("prompt")
		model, _ := cmd.Flags().GetString("model")
		asJSON, _ := cmd.Flags().GetBool("json")
		if name == "" && prompt == "" {
			return fmt.Errorf("give a name, --prompt, or both")
		}

		site, err := c.Create(cmd.Context(), name, prompt, model)
		if err != nil {
			return err
		}

		var build *websites.Build
		var buildErr error
		if prompt != "" {
			b, err := c.Build(cmd.Context(), site.ID, prompt, model)
			if err != nil {
				buildErr = err
			} else {
				build = &b
			}
		}

		if asJSON {
			out := map[string]any{"website": site}
			if build != nil {
				out["build"] = build
			}
			if buildErr != nil {
				out["build_error"] = buildErr.Error()
			}
			b, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}

		fmt.Printf("Created %s (%s)\n", site.Name, site.Slug)
		switch {
		case buildErr != nil && errors.Is(buildErr, websites.ErrBuildsGated):
			fmt.Printf("Build not queued: %s\n", buildErr.Error())
		case buildErr != nil:
			fmt.Printf("Build not queued: %s\n", buildErr.Error())
		case build != nil:
			fmt.Printf("Build queued (%s) — check progress with `memcode websites builds %s`\n", build.ID, site.Slug)
		}
		return nil
	},
}

func init() {
	websitesCreateCmd.Flags().String("prompt", "", "describe the site to build; queues a build after creation")
	websitesCreateCmd.Flags().String("model", "", "chat model id for naming/building (default: catalog default)")
	websitesCreateCmd.Flags().Bool("json", false, "output the created site (and build, if queued) as JSON")
	websitesCmd.AddCommand(websitesCreateCmd)
}
