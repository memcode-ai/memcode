// websites_deps.go — `memcode websites deps` and `memcode websites check`:
// the dependency policy and the production-build check, from the terminal.
// The hosted builder runs `deps check` before it installs anything, so the
// rule a site meets here is the rule it meets on memcode.ai.
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/websites"
)

var websitesDepsCmd = &cobra.Command{
	Use:   "deps",
	Short: "Check or add a website's npm packages",
	Long: `A site can use everything in the memcode kit (React, Tailwind, GSAP, motion,
Lenis, three.js and react-three-fiber, ~28 self-hosted typefaces, and more),
which memcode.ai has preinstalled. Any other npm package can be added if it
is at least 14 days old, widely used, permissively licensed, has no install
scripts and isn't deprecated. Packages install with scripts off, here and on
memcode.ai.`,
}

var websitesDepsCheckCmd = &cobra.Command{
	Use:   "check [dir|name]",
	Short: "Check a site's dependencies against the policy",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := websitesDirArg(args)
		kit, err := depsKit(cmd, dir)
		if err != nil {
			return err
		}
		verdicts, err := websites.Check(cmd.Context(), dir, kit, websites.NewRegistry())
		if err != nil {
			return err
		}
		bad := websites.Problems(verdicts)
		if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
			out := map[string]any{"ok": len(bad) == 0, "packages": verdicts}
			if verdicts == nil {
				out["packages"] = []websites.Verdict{}
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(out); err != nil {
				return err
			}
		} else {
			if len(verdicts) == 0 {
				fmt.Println("Only memcode kit packages: nothing to check.")
			}
			for _, v := range verdicts {
				mark := "✓"
				if !v.OK {
					mark = "✗"
				}
				fmt.Println(mark + " " + v.Summary())
			}
		}
		if len(bad) > 0 {
			return &websites.ErrDependencies{Problems: bad}
		}
		return nil
	},
}

var websitesDepsAddCmd = &cobra.Command{
	Use:   "add <package[@version]> [dir|name]",
	Short: "Add an npm package to a site (kit packages, or any that pass the policy)",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := websitesDirArg(args[1:])
		kit, err := depsKit(cmd, dir)
		if err != nil {
			return err
		}
		v, err := websites.Add(cmd.Context(), dir, args[0], "", kit, websites.NewRegistry())
		if err != nil {
			return err
		}
		if !v.OK {
			return fmt.Errorf("%s", v.Summary())
		}
		fmt.Println("Added " + v.Summary())
		return nil
	},
}

var websitesCheckCmd = &cobra.Command{
	Use:   "check [dir|name]",
	Short: "Check that a site will build on memcode.ai (dependencies + production build)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := websitesDirArg(args)
		kit, err := depsKit(cmd, dir)
		if err != nil {
			return err
		}
		verdicts, err := websites.Check(cmd.Context(), dir, kit, websites.NewRegistry())
		if err != nil {
			return err
		}
		var problems []string
		for _, v := range websites.Problems(verdicts) {
			problems = append(problems, v.Summary())
		}
		fmt.Println("Building the site the way memcode.ai will…")
		r := websites.BuildCheck(cmd.Context(), dir)
		fmt.Println(r.Summary())
		if len(problems) > 0 {
			fmt.Println("\nDependencies memcode.ai won't install:\n- " + strings.Join(problems, "\n- "))
		}
		if !r.OK || len(problems) > 0 {
			return fmt.Errorf("the site isn't ready to publish")
		}
		return nil
	},
}

// depsKit is the kit to judge against: --kit (a package.json, as the hosted
// builder passes its baked template), else the current template from
// memcode.ai when logged in, else the site's own starting package.json.
func depsKit(cmd *cobra.Command, dir string) (websites.Kit, error) {
	if path, _ := cmd.Flags().GetString("kit"); path != "" {
		return websites.ReadKit(path)
	}
	c, _ := websites.New() // nil when logged out: the site's own baseline is used
	return websites.SiteKit(cmd.Context(), c, dir), nil
}

func init() {
	for _, c := range []*cobra.Command{websitesDepsCheckCmd, websitesDepsAddCmd, websitesCheckCmd} {
		c.Flags().String("kit", "", "package.json whose dependencies are the kit (default: the current memcode template)")
	}
	websitesDepsCheckCmd.Flags().Bool("json", false, "print the verdicts as JSON")
	websitesDepsCmd.AddCommand(websitesDepsCheckCmd, websitesDepsAddCmd)
	websitesCmd.AddCommand(websitesDepsCmd, websitesCheckCmd)
}
