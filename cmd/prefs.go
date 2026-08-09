package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/prefs"
)

var prefsCmd = &cobra.Command{
	Use:   "prefs",
	Short: "Run the preference reducer and print the candidate table (debug thresholds)",
	Long: `Scans preference_signal events from the event log, clusters them by axis +
lexical similarity, weighs each cluster with recency decay, and materializes the
candidates into the preference_candidates table — the same reduce step StartChat
runs silently at session start. Prints the resulting candidates with their weight,
signal count, session count, and status so you can see what the system has learned
and how close each candidate is to the promotion bar (weight ≥ 2.0, ≥3 signals,
≥2 sessions).

Confirmed preferences are the standing plaintext rules in .memcode/prefs/*.md;
review them with memcode{command:"preferences"} or by editing the files directly.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, _, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		cands, err := prefs.Reduce(ctx, st)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			fmt.Println("no preference candidates yet — the system learns from your repeated forceful directives (\"always X\", \"never Y\").")
			return nil
		}
		fmt.Printf("%-20s  %-10s  %6s  %6s  %6s  %s\n", "ID", "AXIS", "WEIGHT", "SIGS", "SESS", "TEXT")
		for _, c := range cands {
			status := c.Status
			if status == "" {
				status = "candidate"
			}
			fmt.Printf("%-20s  %-10s  %6.2f  %6d  %6d  [%s] %s\n",
				c.ID, c.Axis, c.Weight, c.SignalCount, c.SessionCount, status, c.Text)
		}
		pending := prefs.PendingPromotions(cands)
		if len(pending) > 0 {
			fmt.Printf("\n%d candidate(s) above the promotion bar (will promote on next StartChat):\n", len(pending))
			for _, p := range pending {
				fmt.Printf("  → [%s] %s (weight %.2f)\n", p.Axis, p.Text, p.Weight)
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(prefsCmd)
}
