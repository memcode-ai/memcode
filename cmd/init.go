package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/sources"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Set up memcode and build the initial model of this project",
	Long: `Creates a .memcode directory holding the state engine, then scans the
project to build its deterministic topology: subsystems, dependencies, ownership
and change hotspots — all from manifests, the directory tree and git history (no
code parsing). Safe to re-run; see also "memcode index" to refresh.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		force, _ := cmd.Flags().GetBool("force")

		// Resolve the canonical root (git toplevel) so state never depends on the
		// directory init happens to be run from.
		root, src, err := config.Resolve(".")
		if err != nil {
			return err
		}
		created, err := config.Init(root, force)
		if err != nil {
			return err
		}
		if created {
			fmt.Printf("created %s/ (memcode's local state — self-ignored via %s/.gitignore)\n", config.DirName, config.DirName)
		}
		fmt.Printf("repo root: %s  (%s)\n", root, src)
		fmt.Printf("state db:  %s\n\n", storePath(root))

		st, err := store.Open(ctx, storePath(root))
		if err != nil {
			return err
		}
		defer st.Close()

		res, err := structure.Scan(ctx, st, root)
		if err != nil {
			return err
		}
		srcs, _ := sources.Discover(ctx, root)
		_ = sources.Persist(ctx, st, srcs)
		if _, err := events.Append(ctx, st, events.KindNote, "init",
			map[string]any{"subsystems": len(res.Subsystems), "sources": len(srcs)}); err != nil {
			return err
		}

		printTopology(res)
		if len(srcs) > 0 {
			fmt.Printf("\nFound %d instruction/doc source(s) (CLAUDE.md, .cursor, AGENTS.md, README, …) — see `memcode sources`.\n", len(srcs))
		}
		fmt.Println()
		fmt.Println("Next: `memcode objective add \"<goal>\"` to record what you're working on.")
		return nil
	},
}

// printTopology renders the deterministic subsystem map produced by a scan.
func printTopology(res structure.Result) {
	if len(res.Subsystems) == 0 {
		fmt.Println("No subsystems detected (no package/module manifests found).")
		return
	}
	fmt.Printf("Discovered %d subsystem(s):\n", len(res.Subsystems))
	for _, s := range res.Subsystems {
		owner := ""
		if len(s.Owners) > 0 {
			owner = "  ·  " + s.Owners[0]
		}
		fmt.Printf("  • %-28s %-7s %d commit(s)%s\n", s.Key, s.Ecosystem, s.Commits, owner)
	}
	if len(res.Deps) > 0 {
		fmt.Printf("\nDependencies:\n")
		for _, d := range res.Deps {
			fmt.Printf("  %s → %s\n", d.From, d.To)
		}
	}
}

func init() {
	initCmd.Flags().BoolP("force", "f", false, "overwrite existing configuration")
	rootCmd.AddCommand(initCmd)
}
