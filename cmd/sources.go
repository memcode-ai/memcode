package cmd

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/sources"
)

var sourcesCmd = &cobra.Command{
	Use:   "sources",
	Short: "Show instruction/memory/doc sources found in the repo (doctrine candidates)",
	Long: `Lists the instruction and memory artifacts left by AI tools and humans —
CLAUDE.md, .claude/, .cursor/rules, AGENTS.md, copilot/windsurf configs, README,
docs, ADRs — each scoped to the directory it governs and flagged when the code
has changed more recently than the source (likely stale).

These are EVIDENCE (doctrine candidates), not automatically truth.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, _, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		srcs, err := sources.Load(ctx, st)
		if err != nil {
			return err
		}
		if len(srcs) == 0 {
			fmt.Println("No instruction/doc sources found. (Run `memcode init` or `memcode index`.)")
			return nil
		}
		sort.Slice(srcs, func(i, j int) bool {
			if srcs[i].Stale != srcs[j].Stale {
				return !srcs[i].Stale // current first
			}
			return srcs[i].Path < srcs[j].Path
		})

		fmt.Printf("%d source(s) found:\n\n", len(srcs))
		fmt.Printf("  %-36s %-14s %-10s %-12s %s\n", "path", "kind", "scope", "updated", "status")
		for _, s := range srcs {
			status := "current"
			if s.Stale {
				status = "STALE?"
			}
			scope := s.Scope
			if scope == "." {
				scope = "repo"
			}
			fmt.Printf("  %-36s %-14s %-10s %-12s %s\n", trunc(s.Path, 36), s.Kind, trunc(scope, 10), s.GitDate, status)
		}

		var stale int
		for _, s := range srcs {
			if s.Stale {
				stale++
			}
		}
		if stale > 0 {
			fmt.Printf("\n⚠ %d source(s) may be stale (code changed after them). Run `memcode why <path>` or reconcile in `learn`.\n", stale)
		}
		return nil
	},
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func init() {
	rootCmd.AddCommand(sourcesCmd)
}
