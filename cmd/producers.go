package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/gitlog"
	"github.com/memcode-ai/memcode/internal/producer"
	"github.com/memcode-ai/memcode/internal/structure"
)

var producersCmd = &cobra.Command{
	Use:   "producers",
	Short: "Show who/what produced recent work (human vs AI tools), overall and by subsystem",
	Long: `Attributes recent commits to a producer — human, claude-code, cursor,
codex, copilot — and tallies them overall and per subsystem, so you can see at a
glance whether an area is human-driven or agent-driven.

Attribution is a confidence-tagged CANDIDATE, not truth: a human often commits an
agent's work. Treat it as a signal.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		n, _ := cmd.Flags().GetInt("limit")

		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		topo, err := structure.Load(ctx, st)
		if err != nil {
			return err
		}
		commits := gitlog.Recent(ctx, cfg.Root, "", n)
		if len(commits) == 0 {
			fmt.Println("No commit history found.")
			return nil
		}
		files := recentFiles(ctx, cfg.Root, n)

		overall := map[string]int{}
		// subsystem -> producer -> commit count
		bySub := map[string]map[string]int{}

		for _, c := range commits {
			prod := string(producer.Classify(c.Author, c.AuthorEmail, c.Subject, c.Body).Producer)
			overall[prod]++

			subs := map[string]bool{}
			for _, f := range files[c.Hash] {
				if s, ok := structure.ContainingSubsystem(topo.Subsystems, f); ok {
					subs[s.Key] = true
				}
			}
			for key := range subs {
				if bySub[key] == nil {
					bySub[key] = map[string]int{}
				}
				bySub[key][prod]++ // one commit counts once per subsystem
			}
		}

		fmt.Printf("Recent producers (last %d commit(s)):\n", len(commits))
		for _, kv := range rank(overall) {
			fmt.Printf("  %-14s %d\n", kv.k, kv.v)
		}

		if len(bySub) > 0 {
			fmt.Printf("\nBy subsystem:\n")
			keys := make([]string, 0, len(bySub))
			for k := range bySub {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				var parts []string
				for _, kv := range rank(bySub[k]) {
					parts = append(parts, fmt.Sprintf("%s %d", kv.k, kv.v))
				}
				fmt.Printf("  %-28s %s\n", k, strings.Join(parts, ", "))
			}
		}
		return nil
	},
}

type kv struct {
	k string
	v int
}

func rank(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].v != out[j].v {
			return out[i].v > out[j].v
		}
		return out[i].k < out[j].k
	})
	return out
}

// recentFiles maps each recent commit (short hash) to the files it changed.
func recentFiles(ctx context.Context, root string, n int) map[string][]string {
	out := map[string][]string{}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "log", "-n", fmt.Sprintf("%d", n),
		"--name-only", "--format=\x01%h")
	res, err := cmd.Output()
	if err != nil {
		return out
	}
	var cur string
	for _, line := range strings.Split(string(res), "\n") {
		if strings.HasPrefix(line, "\x01") {
			cur = strings.TrimPrefix(line, "\x01")
			continue
		}
		if line != "" && cur != "" {
			out[cur] = append(out[cur], line)
		}
	}
	return out
}

func init() {
	producersCmd.Flags().Int("limit", 50, "number of recent commits to analyze")
	rootCmd.AddCommand(producersCmd)
}
