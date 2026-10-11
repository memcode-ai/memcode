package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/memcode-ai/memcode/internal/authflow"
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
  memcode websites delete <slug>   Delete a website
  memcode websites update <slug>   Rename it or change its model
  memcode websites open <slug>     Open the live site (or --preview the draft)
  memcode websites dev [slug|dir]  Run its real Vite dev server locally
  memcode websites assets ...      Manage its uploaded, rebuild-proof assets

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

var websitesDeleteCmd = &cobra.Command{
	Use:   "delete <slug>",
	Short: "Delete a website",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		slug := args[0]
		site, err := c.Resolve(cmd.Context(), slug)
		if err != nil {
			return err
		}
		yes, _ := cmd.Flags().GetBool("yes")
		if !yes {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("refusing to delete without confirmation — pass --yes")
			}
			fmt.Printf("Delete website %s? It stops serving immediately. [y/N] ", slug)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
				fmt.Println("Cancelled.")
				return nil
			}
		}
		if err := c.Delete(cmd.Context(), site.ID); err != nil {
			return err
		}
		fmt.Printf("Deleted %s\n", slug)
		return nil
	},
}

var websitesUpdateCmd = &cobra.Command{
	Use:   "update <slug>",
	Short: "Rename a website or change its model",
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
		var name, model *string
		if cmd.Flags().Changed("name") {
			v, _ := cmd.Flags().GetString("name")
			name = &v
		}
		if cmd.Flags().Changed("model") {
			v, _ := cmd.Flags().GetString("model")
			model = &v
		}
		updated, err := c.Update(cmd.Context(), site.ID, name, model)
		if err != nil {
			return err
		}
		fmt.Printf("Updated %s\n", updated.Slug)
		return nil
	},
}

var websitesOpenCmd = &cobra.Command{
	Use:   "open <slug>",
	Short: "Open a website in your browser",
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
		preview, _ := cmd.Flags().GetBool("preview")
		var u string
		if preview {
			u, err = c.Preview(cmd.Context(), site.ID)
			if err != nil {
				return err
			}
		} else {
			if site.LiveURL == "" {
				return fmt.Errorf("%s has no live URL yet — publish it first, or pass --preview", args[0])
			}
			u = site.LiveURL
		}
		fmt.Println(u)
		if err := authflow.OpenBrowser(u); err != nil {
			fmt.Println("(could not open a browser — use the URL above)")
		}
		return nil
	},
}

// memcode websites assets — manage the current pulled site's durable
// uploads (never touched by a rebuild), resolved from cwd via ReadMeta —
// same convention as push/publish/unpublish.
var websitesAssetsCmd = &cobra.Command{
	Use:   "assets",
	Short: "Manage this site's uploaded assets (served at /assets/<name>, untouched by rebuilds)",
}

var websitesAssetsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List this site's uploaded assets",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		meta, err := websites.ReadMeta(websitesDirArg(nil))
		if err != nil {
			return err
		}
		list, err := c.ListAssets(cmd.Context(), meta.SiteID)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("No assets uploaded yet.")
			return nil
		}
		for _, a := range list {
			fmt.Printf("%-30s %10d bytes  %s\n", a.Name, a.Size, a.URL)
		}
		return nil
	},
}

var websitesAssetsUploadCmd = &cobra.Command{
	Use:   "upload <file>",
	Short: "Upload a file as a site asset, served at /assets/<name>",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		meta, err := websites.ReadMeta(websitesDirArg(nil))
		if err != nil {
			return err
		}
		name, _ := cmd.Flags().GetString("name")
		asset, err := c.UploadAsset(cmd.Context(), meta.SiteID, args[0], name)
		if err != nil {
			return err
		}
		fmt.Printf("Uploaded %s (%d bytes) — %s\n", asset.Name, asset.Size, asset.URL)
		return nil
	},
}

var websitesAssetsDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete an uploaded asset",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := websites.New()
		if err != nil {
			return err
		}
		meta, err := websites.ReadMeta(websitesDirArg(nil))
		if err != nil {
			return err
		}
		if err := c.DeleteAsset(cmd.Context(), meta.SiteID, args[0]); err != nil {
			return err
		}
		fmt.Printf("Deleted %s\n", args[0])
		return nil
	},
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
	websitesAssetsCmd.AddCommand(websitesAssetsListCmd, websitesAssetsUploadCmd, websitesAssetsDeleteCmd)
	websitesAssetsUploadCmd.Flags().String("name", "", "store under this name instead of the file's own base name")
	websitesDeleteCmd.Flags().Bool("yes", false, "delete without prompting (required when stdin is not a TTY)")
	websitesUpdateCmd.Flags().String("name", "", "new display name")
	websitesUpdateCmd.Flags().String("model", "", "new chat model id")
	websitesOpenCmd.Flags().Bool("preview", false, "open the draft preview instead of the live site")
	websitesCmd.AddCommand(websitesListCmd, websitesPullCmd, websitesPushCmd,
		websitesPublishCmd, websitesUnpublishCmd, websitesDeleteCmd, websitesUpdateCmd,
		websitesOpenCmd, websitesAssetsCmd)
	rootCmd.AddCommand(websitesCmd)
}
