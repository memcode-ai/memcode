package cmd

import (
	"fmt"
	"time"

	"github.com/memcode-ai/memcode/internal/personal"
	"github.com/spf13/cobra"
)

var personalTriggersCmd = &cobra.Command{Use: "triggers", Short: "Manage persistent wake triggers"}

var personalTriggersAddCmd = &cobra.Command{
	Use: "add <agent> <kind> <spec>", Args: cobra.ExactArgs(3),
	Short: "Add a wake trigger (interval 5m | cron \"0 * * * *\" | one-shot RFC3339)",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, _, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		kind, spec := args[1], args[2]
		kindMap := map[string]string{"interval": "interval", "cron": "cron", "one-shot": "one_shot"}
		dbKind, ok := kindMap[kind]
		if !ok {
			return fmt.Errorf("kind must be interval, cron, or one-shot")
		}
		now := time.Now().UTC()
		next, err := personal.NextDue(dbKind, spec, now)
		if err != nil {
			return fmt.Errorf("bad spec: %w", err)
		}
		id := fmt.Sprintf("trig-%s-%d", dbKind, now.Unix())
		if err := st.CreateTrigger(cmd.Context(), personal.Trigger{ID: id, ObjectiveID: "primary", Kind: dbKind, Spec: spec, NextDueAt: &next}); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Trigger %s added; next wake %s.\n", id, next.Format(time.RFC3339))
		return nil
	},
}

var personalTriggersListCmd = &cobra.Command{
	Use: "list <agent>", Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		st, _, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		trigs, err := st.ListTriggers(cmd.Context())
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(trigs) == 0 {
			fmt.Fprintln(out, "no triggers — the agent only wakes on `personal run` or answered interactions")
			return nil
		}
		for _, t := range trigs {
			next := "—"
			if t.NextDueAt != nil {
				next = t.NextDueAt.Format(time.RFC3339)
			}
			fmt.Fprintf(out, "- %s: %s %q next=%s [%s]\n", t.ID, t.Kind, t.Spec, next, t.Status)
		}
		return nil
	},
}

func triggerSetStatus(s string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		st, _, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		res, err := st.DB().ExecContext(cmd.Context(), `UPDATE triggers SET status=?,updated_at=? WHERE id=?`, s, time.Now().UTC().Format(time.RFC3339Nano), args[1])
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no trigger %q for %s", args[1], args[0])
		}
		fmt.Fprintf(cmd.OutOrStdout(), "trigger %s: %s\n", args[1], s)
		return nil
	}
}

func init() {
	pause := &cobra.Command{Use: "pause <agent> <trigger-id>", Args: cobra.ExactArgs(2), RunE: triggerSetStatus("paused")}
	resume := &cobra.Command{Use: "resume <agent> <trigger-id>", Args: cobra.ExactArgs(2), RunE: triggerSetStatus("enabled")}
	personalTriggersCmd.AddCommand(personalTriggersAddCmd, personalTriggersListCmd, pause, resume)
}
