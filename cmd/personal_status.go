package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/memcode-ai/memcode/internal/personal"
	"github.com/spf13/cobra"
)

var personalHistoryCmd = &cobra.Command{
	Use: "history <name>", Args: cobra.ExactArgs(1),
	Short: "Show recent runs and journaled actions",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, _, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		ctx := cmd.Context()
		w := cmd.OutOrStdout()
		runs, err := st.ListRuns(ctx, "primary", 10)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "RUNS (%d most recent):\n", len(runs))
		for _, r := range runs {
			fmt.Fprintf(w, "  %s [%s] %s\n", r.ID, r.Status, r.CreatedAt.Format(time.RFC3339))
		}
		actions, err := st.ListActions(ctx, "primary", 20)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "ACTIONS (%d most recent):\n", len(actions))
		for _, a := range actions {
			fmt.Fprintf(w, "  %s %s %s → %s (policy %s)\n", a.CreatedAt.Format("15:04:05"), a.Kind, a.Target, a.Status, shortHash(a.PolicyHash))
		}
		return nil
	},
}

var personalDoctorCmd = &cobra.Command{
	Use: "doctor <name>", Args: cobra.ExactArgs(1),
	Short: "Check a Personal Agent's home, policy, and runtime health",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, home, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		ctx := cmd.Context()
		w := cmd.OutOrStdout()
		ok := true
		check := func(name string, good bool, detail string) {
			mark := "ok"
			if !good {
				mark = "FAIL"
				ok = false
			}
			fmt.Fprintf(w, "  [%s] %s: %s\n", mark, name, detail)
		}
		for _, d := range []string{"policies", "workspace/generated", "workspace/scratch", "runs", ".memcode/sessions"} {
			_, err := os.Stat(filepath.Join(home, d))
			check("dir "+d, err == nil, filepath.Join(home, d))
		}
		obj, hasObj, _ := st.GetObjective(ctx, "primary")
		check("objective", hasObj, obj.Description)
		pol, hasPol, _ := st.ApprovedPolicy(ctx, "primary")
		check("approved policy", hasPol, func() string {
			if hasPol {
				return fmt.Sprintf("v%d", pol.Version) + " " + shortHash(pol.Hash)
			}
			return "none — consequential work blocked"
		}())
		if _, err := personal.InitializeGeneratedWorkspace(home); err != nil {
			check("generated workspace", false, err.Error())
		} else {
			check("generated workspace", true, "git initialized")
		}
		// Sandbox availability is informational, not a failure: on platforms without
		// bwrap the runner fails closed for generated code by design (safe default).
		fmt.Fprintf(w, "  [info] sandbox: %s\n", sandboxNote())
		trigs, _ := st.ListTriggers(ctx)
		fmt.Fprintf(w, "  triggers: %d, ", len(trigs))
		pend, _ := st.PendingInteractions(ctx, args[0])
		fmt.Fprintf(w, "pending interactions: %d\n", len(pend))
		if !ok {
			return fmt.Errorf("doctor found problems")
		}
		return nil
	},
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
func sandboxNote() string {
	if personal.SandboxAvailable() {
		return "hardened (bwrap)"
	}
	return "no bwrap — generated code runs fail-closed unless explicitly approved"
}

func init() {
	personalCmd.AddCommand(personalHistoryCmd, personalDoctorCmd)
}
