package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/membench"
)

var benchCmd = &cobra.Command{
	Use:    "bench",
	Short:  "Internal benchmarks",
	Hidden: true,
}

var (
	benchDataset string
	benchAdapter string
	benchMode    string
	benchLimit   int
	benchTopK    int
	benchData    string
	benchKeep    bool
	benchFacts   bool
	benchSplit   string
)

func benchAdapters(name string) ([]membench.Adapter, error) {
	if name == "hybrid" {
		h, err := membench.NewHybridAdapter()
		if err != nil {
			return nil, err
		}
		return []membench.Adapter{h}, nil
	}
	return membench.Adapters(name), nil
}

var benchMemoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Score session-memory retrieval against LongMemEval / LoCoMo (no model calls)",
	RunE: func(cmd *cobra.Command, args []string) error {
		datasets := []string{benchDataset}
		if benchDataset == "all" {
			datasets = []string{"longmemeval-s", "locomo"}
		}
		work := membench.CacheDir()
		if err := os.MkdirAll(work, 0o755); err != nil {
			return err
		}
		for _, dsName := range datasets {
			ds, err := membench.Load(dsName, benchData)
			if err != nil {
				return err
			}
			ds, err = membench.SplitQuestions(ds, benchSplit)
			if err != nil {
				return err
			}
			if benchFacts {
				if err := membench.GenerateFacts(ds, benchLimit); err != nil {
					return err
				}
			}
			ads, err := benchAdapters(benchAdapter)
			if err != nil {
				return err
			}
			for _, ad := range ads {
				if benchMode == "qa" {
					res, err := membench.RunQA(ds, ad, work, benchLimit, benchTopK)
					if err != nil {
						return err
					}
					fmt.Print(res.Render())
					continue
				}
				res, err := membench.Run(ds, ad, work, benchLimit, benchKeep)
				if err != nil {
					return err
				}
				fmt.Print(res.Render())
				if path, err := res.WriteLog(work + "/logs"); err == nil {
					fmt.Printf("  retrieval log: %s\n\n", path)
				}
			}
		}
		return nil
	},
}

func init() {
	benchMemoryCmd.Flags().StringVar(&benchDataset, "dataset", "all", "longmemeval-s | locomo | all")
	benchMemoryCmd.Flags().StringVar(&benchAdapter, "adapter", "all", "legacy | product | ablation | bm25 | bm25+time | bm25v2 | bm25v2+time | hybrid | all")
	benchMemoryCmd.Flags().StringVar(&benchMode, "mode", "retrieval", "retrieval (no model calls) | qa (answer + judge; needs MEMBENCH_QA_* env)")
	benchMemoryCmd.Flags().IntVar(&benchTopK, "topk", 6, "qa mode: retrieved sessions per prompt")
	benchMemoryCmd.Flags().IntVar(&benchLimit, "limit", 0, "cap the number of questions (0 = all)")
	benchMemoryCmd.Flags().StringVar(&benchData, "data", "", "path to a local dataset JSON (skips download)")
	benchMemoryCmd.Flags().BoolVar(&benchKeep, "keep", false, "keep the ingested .memcode roots for inspection")
	benchMemoryCmd.Flags().BoolVar(&benchFacts, "facts", false, "generate + ingest per-session facts records (needs MEMBENCH_QA_* env; cached)")
	benchMemoryCmd.Flags().StringVar(&benchSplit, "split", "all", "question split: even (tune) | odd (holdout) | all")
	benchCmd.AddCommand(benchMemoryCmd)
	rootCmd.AddCommand(benchCmd)
}
