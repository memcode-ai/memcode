package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/objectives"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
)

var mapCmd = &cobra.Command{
	Use:   "map [subsystem]",
	Short: "Show the deterministic topology of the project (no model)",
	Long: `Renders the ground-truth map of the repository — subsystems, their
dependencies, ownership, change hotspots and current objectives — entirely from
deterministic signals (no model, no network). With a subsystem argument it
drills down toward that subsystem's details and files.

This is the floor that the model-backed "understand" is checked against: when
"understand" looks wrong, look here.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		res, err := structure.Load(ctx, st)
		if err != nil {
			return err
		}
		if len(res.Subsystems) == 0 {
			return fmt.Errorf("no topology yet — run `memcode init`")
		}

		if len(args) == 1 {
			return mapSubsystem(cfg.Root, res, args[0])
		}
		return mapOverview(ctx, st, res)
	},
}

func mapOverview(ctx context.Context, st store.Store, res structure.Result) error {
	fmt.Printf("%s\n", filepath.Base(res.Root))
	if !res.GeneratedAt.IsZero() {
		fmt.Printf("scanned %s\n", res.GeneratedAt.Local().Format("2006-01-02 15:04"))
	}

	depCount := map[string]int{}
	for _, d := range res.Deps {
		depCount[d.From]++
	}

	// Rank by churn-weighted recent activity (lines changed + commits + active
	// days, last 30d) — where the work actually is now, and weighted by depth so
	// many tiny commits in one area don't drown out fewer, deeper changes.
	subs := structure.ByHotness(res.Subsystems)
	totalRecent := 0
	for _, s := range subs {
		totalRecent += s.Recent
	}

	heading := "Subsystems"
	if totalRecent > 0 {
		heading = "Subsystems (hottest last 30d first: churn · commits · active days)"
	}
	fmt.Printf("\n%s (%d):\n", heading, len(subs))
	for _, s := range subs {
		extra := []string{}
		if s.RecentChurn > 0 {
			extra = append(extra, fmt.Sprintf("~%d lines/30d", s.RecentChurn))
		}
		if s.Recent > 0 {
			extra = append(extra, fmt.Sprintf("%d recent", s.Recent))
		}
		if s.RecentDays > 0 {
			extra = append(extra, fmt.Sprintf("%dd active", s.RecentDays))
		}
		if s.Commits > 0 {
			extra = append(extra, fmt.Sprintf("%d total", s.Commits))
		}
		if len(s.Owners) > 0 {
			extra = append(extra, s.Owners[0])
		}
		if n := depCount[s.Key]; n > 0 {
			extra = append(extra, fmt.Sprintf("→%d deps", n))
		}
		tail := ""
		if len(extra) > 0 {
			tail = "  (" + strings.Join(extra, ", ") + ")"
		}
		fmt.Printf("  • %-28s %-6s%s\n", s.Key, s.Ecosystem, tail)
	}

	if len(res.Deps) > 0 {
		fmt.Printf("\nDependencies:\n")
		for _, d := range res.Deps {
			fmt.Printf("  %s → %s\n", d.From, d.To)
		}
	}

	if obj, err := objectives.New(st).Current(ctx); err == nil && len(obj) > 0 {
		fmt.Printf("\nCurrent objectives:\n")
		for _, o := range obj {
			fmt.Printf("  %s  [%s] %s\n", o.ID, o.Status, o.Title)
		}
	}

	if len(res.Docs) > 0 {
		fmt.Printf("\nKey docs: %s\n", strings.Join(res.Docs, ", "))
	}
	fmt.Printf("\nDrill down with: memcode map <subsystem>\n")
	return nil
}

func mapSubsystem(root string, res structure.Result, query string) error {
	sub, ok := findSubsystem(res.Subsystems, query)
	if !ok {
		keys := make([]string, len(res.Subsystems))
		for i, s := range res.Subsystems {
			keys[i] = s.Key
		}
		return fmt.Errorf("no subsystem matching %q. Known: %s", query, strings.Join(keys, ", "))
	}

	fmt.Printf("%s\n", sub.Key)
	if sub.Package != "" {
		fmt.Printf("  package   %s\n", sub.Package)
	}
	fmt.Printf("  ecosystem %s  (%s)\n", sub.Ecosystem, sub.Manifest)
	if sub.Commits > 0 {
		fmt.Printf("  activity  %d commit(s)\n", sub.Commits)
	}
	if len(sub.Owners) > 0 {
		fmt.Printf("  owners    %s\n", strings.Join(sub.Owners, ", "))
	}
	if len(sub.Docs) > 0 {
		fmt.Printf("  docs      %s\n", strings.Join(sub.Docs, ", "))
	}

	var dependsOn, dependedBy []string
	for _, d := range res.Deps {
		if d.From == sub.Key {
			dependsOn = append(dependsOn, d.To)
		}
		if d.To == sub.Key {
			dependedBy = append(dependedBy, d.From)
		}
	}
	if len(dependsOn) > 0 {
		fmt.Printf("\n  depends on:    %s\n", strings.Join(dependsOn, ", "))
	}
	if len(dependedBy) > 0 {
		fmt.Printf("  depended on by: %s\n", strings.Join(dependedBy, ", "))
	}

	if entries := topLevelEntries(filepath.Join(root, sub.Key)); len(entries) > 0 {
		fmt.Printf("\n  contents:\n")
		for _, e := range entries {
			fmt.Printf("    %s\n", e)
		}
	}
	return nil
}

// findSubsystem resolves a query to a subsystem by exact key, then by basename.
func findSubsystem(subs []structure.Subsystem, query string) (structure.Subsystem, bool) {
	for _, s := range subs {
		if s.Key == query {
			return s, true
		}
	}
	for _, s := range subs {
		if filepath.Base(s.Key) == query || strings.HasSuffix(s.Key, "/"+query) {
			return s, true
		}
	}
	return structure.Subsystem{}, false
}

// topLevelEntries lists the immediate children of dir (the start of a drill-down
// toward files), skipping noise, capped for readability.
func topLevelEntries(dir string) []string {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist" {
			continue
		}
		if de.IsDir() {
			name += "/"
		}
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) > 24 {
		out = append(out[:24], fmt.Sprintf("… (%d more)", len(out)-24))
	}
	return out
}

func init() {
	rootCmd.AddCommand(mapCmd)
}
