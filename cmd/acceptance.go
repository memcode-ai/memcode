package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/acceptance"
	"github.com/memcode-ai/memcode/internal/store"
)

func strPayload(p json.RawMessage, key string) string {
	if len(p) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(p, &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

var acceptanceCmd = &cobra.Command{
	Use:   "acceptance",
	Short: "Did the agent's work survive? Reconcile sessions as accepted/corrected/rejected via git",
	Long: `Compares each finished agent session's file changes against the current git
state and records the verdict:

  accepted   the agent's files were committed substantially intact
  corrected  the files were manually changed after the agent
  rejected   the patch was reverted / reset / discarded

This is memcode's "did my work survive contact with the human?" loop — objective
git evidence most agents never look at. Runs opportunistically during other
commands too; this surfaces it directly. No-op outside a git repo.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		newly, err := acceptance.Reconcile(ctx, st, cfg.Root)
		if err != nil {
			return err
		}
		for _, r := range newly {
			fmt.Printf("● %s → %s (%s) — %s\n", r.SessionID, r.Outcome, r.Confidence, r.Evidence)
		}

		// Show the current picture (most recent outcomes).
		evs, err := st.ListEvents(ctx, store.EventFilter{Kinds: []string{"session_outcome"}})
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			fmt.Println("no reconciled sessions yet — run the agent, then commit/revert its work.")
			return nil
		}
		fmt.Println("\nsession outcomes:")
		for _, e := range evs {
			fmt.Printf("  %s  %-9s  %s\n",
				strPayload(e.Payload, "session_id"), strPayload(e.Payload, "outcome"), strPayload(e.Payload, "evidence"))
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(acceptanceCmd)
}
