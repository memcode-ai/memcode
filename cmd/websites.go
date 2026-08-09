package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/websites"
)

// memcode websites — manage AI-built static sites (the www Websites feature)
// from the terminal. The pulled repo carries the same MEMCODE.md + .memcode/
// doctrine the hosted builder uses, so plain `memcode` inside it IS website
// mode — interactive, with real HITL — and `push` syncs the preview back.
var websitesCmd = &cobra.Command{
	Use:   "websites",
	Short: "List, pull, push, and publish your memcode websites",
	Long: `Work on your memcode Websites locally.

  memcode websites list            Your org's sites and their status
  memcode websites pull <slug>     Clone a site's repo into ./<slug>
  memcode websites push [dir]      Upload local changes; rebuilds the preview
  memcode websites publish [dir]   Promote the preview to the live site
  memcode websites unpublish [dir] Take the live site offline

After pull, run ` + "`memcode`" + ` inside the directory to iterate on the site with
the same constrained website agent the web app uses.`,
	RunE: func(cmd *cobra.Command, args []string) error { return runWebsitesList(cmd) },
}

var websitesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your org's websites",
	RunE:  func(cmd *cobra.Command, args []string) error { return runWebsitesList(cmd) },
}

var websitesPullCmd = &cobra.Command{
	Use:   "pull <slug>",
	Short: "Clone a site's repo into ./<slug>",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		slug := args[0]
		site, err := c.Pull(cmd.Context(), slug, "./"+slug)
		if err != nil {
			return err
		}
		fmt.Printf("Cloned %s to ./%s\n", site.Name, slug)
		fmt.Printf("Run `memcode` in that directory to iterate on the site, then `memcode websites push` to update the preview.\n")
		return nil
	},
}

var websitesPushCmd = &cobra.Command{
	Use:   "push [dir]",
	Short: "Upload local changes and rebuild the draft preview",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		dir := websitesDirArg(args)
		site, err := c.Push(cmd.Context(), dir)
		if err != nil {
			return err
		}
		fmt.Printf("Pushed %s — a rebuild was queued; the preview will refresh shortly.\n", site.Slug)
		return nil
	},
}

var websitesPublishCmd = &cobra.Command{
	Use:   "publish [dir]",
	Short: "Promote the draft preview to the live site",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		meta, err := websites.ReadMeta(websitesDirArg(args))
		if err != nil {
			return err
		}
		url, err := c.Publish(cmd.Context(), meta.SiteID)
		if err != nil {
			return err
		}
		if url != "" {
			fmt.Printf("Published — live at %s\n", url)
		} else {
			fmt.Printf("Published %s\n", meta.Slug)
		}
		return nil
	},
}

var websitesUnpublishCmd = &cobra.Command{
	Use:   "unpublish [dir]",
	Short: "Take the live site offline",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		meta, err := websites.ReadMeta(websitesDirArg(args))
		if err != nil {
			return err
		}
		if err := c.Unpublish(cmd.Context(), meta.SiteID); err != nil {
			return err
		}
		fmt.Printf("Unpublished %s\n", meta.Slug)
		return nil
	},
}

// websitesDirArg resolves the optional [dir] positional, defaulting to cwd.
func websitesDirArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func runWebsitesList(cmd *cobra.Command) error {
	c, err := websites.New()
	if err != nil {
		return err
	}
	sites, err := c.List(cmd.Context())
	if err != nil {
		return err
	}
	if len(sites) == 0 {
		fmt.Println("No websites yet — create one at memcode.ai/websites")
		return nil
	}
	fmt.Println(websites.RenderList(sites))
	return nil
}

func init() {
	websitesCmd.AddCommand(websitesListCmd, websitesPullCmd, websitesPushCmd,
		websitesPublishCmd, websitesUnpublishCmd)
	rootCmd.AddCommand(websitesCmd)
}
