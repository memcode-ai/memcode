package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/recall"
)

var recallCmd = &cobra.Command{
	Use:   "recall <question>",
	Short: "Find where something was decided or documented (NL recall over prose)",
	Long: `Searches the repository's prose memory — source docs (CLAUDE.md, READMEs,
AGENTS.md, docs, ADRs), adjudicated claims, and human decisions — and returns the
passages most relevant to your question. Use it to answer "where did we decide
X?" or "what's our policy on Y?".

Recall runs fully offline and free: a local BM25 lexical ranker, no network and
no API key, boosted by memcode doctrine — current claims and instruction files
outrank generic docs, recent reality outranks old, and stale/conflicted material
is down-weighted unless your question is about history or conflicts. It matches
on wording, not deep meaning; for exact code questions ("where is this handler /
symbol / test?") use ` + "`memcode explore`" + ` or the agent's tools instead. A
hosted semantic provider may be added later only if an eval proves a real gap.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		question := strings.Join(args, " ")
		k, _ := cmd.Flags().GetInt("k")
		path, _ := cmd.Flags().GetString("path")

		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		hits, err := recall.Recall(ctx, st, cfg.Root, question, path, k)
		if err != nil {
			return err
		}
		if len(hits) == 0 {
			fmt.Println("nothing matched in the prose corpus — try different words, or run `memcode sources` / `memcode learn` first.")
			return nil
		}
		for _, h := range hits {
			fmt.Printf("%.3f  %s\n", h.Score, h.Chunk.Source)
			fmt.Printf("%s\n\n", indentSnippet(h.Chunk.Text))
		}
		return nil
	},
}

// indentSnippet trims and indents a passage, capping very long ones.
func indentSnippet(s string) string {
	s = strings.TrimSpace(s)
	const max = 400
	if len(s) > max {
		s = s[:max] + " …"
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

func init() {
	recallCmd.Flags().Int("k", 5, "number of passages to return")
	recallCmd.Flags().String("path", "", "boost passages governing this path/subsystem")
	rootCmd.AddCommand(recallCmd)
}
