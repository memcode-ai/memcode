package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/personal"
	"github.com/spf13/cobra"
)

var personalPolicyCmd = &cobra.Command{Use: "policy", Short: "Manage delegation policies"}

var personalPolicySetCmd = &cobra.Command{
	Use: "set <agent> <policy-file>", Args: cobra.ExactArgs(2),
	Short: "Stage a new draft policy (JSON) for review",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, home, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		var doc personal.DelegationPolicy
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("policy is not valid DelegationPolicy JSON: %w", err)
		}
		canon, hash, err := personal.CanonicalPolicy(doc)
		if err != nil {
			return err
		}
		ver, err := st.NextPolicyVersion(cmd.Context(), "primary")
		if err != nil {
			return err
		}
		p := personal.Policy{ID: "policy-" + hash[:8], ObjectiveID: "primary", Version: ver, Document: canon, Hash: hash, Status: "draft"}
		if err := st.InsertPolicy(cmd.Context(), p); err != nil {
			return err
		}
		path := home + "/policies/" + hash + ".json"
		if err := atomicfile.WriteFile(path, canon, 0o600); err != nil {
			return err
		}
		_ = personal.WriteConfigMirror(cmd.Context(), home, st)
		fmt.Fprintf(cmd.OutOrStdout(), "Draft policy v%d staged (hash %s…). Review with `personal policy show %s` then approve with `personal approve-policy %s %s`.\n", ver, hash[:12], args[0], args[0], hash)
		return nil
	},
}

var personalPolicyShowCmd = &cobra.Command{
	Use: "show <agent> [hash]", Args: cobra.RangeArgs(1, 2),
	Short: "Show the approved policy (or a specific one by hash)",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, _, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		if len(args) == 2 {
			pols, err := st.ListPolicies(cmd.Context(), "primary")
			if err != nil {
				return err
			}
			for _, p := range pols {
				if p.Hash == args[1] || strings.HasPrefix(p.Hash, args[1]) {
					fmt.Fprintf(cmd.OutOrStdout(), "policy v%d [%s] hash=%s\n%s\n", p.Version, p.Status, p.Hash, string(p.Document))
					return nil
				}
			}
			return fmt.Errorf("no policy matching %q", args[1])
		}
		p, ok, err := st.ApprovedPolicy(cmd.Context(), "primary")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(cmd.OutOrStdout(), "no approved policy — consequential work is blocked")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "approved policy v%d hash=%s approved_at=%s\n%s\n", p.Version, p.Hash, p.ApprovedAt, string(p.Document))
		return nil
	},
}

var personalApprovePolicyCmd = &cobra.Command{
	Use: "approve-policy <agent> <hash>", Args: cobra.ExactArgs(2),
	Short: "Approve a staged draft policy by its hash",
	RunE: func(cmd *cobra.Command, args []string) error {
		st, home, err := personalStoreHome(cmd, args[0])
		if err != nil {
			return err
		}
		defer st.Close()
		pols, err := st.ListPolicies(cmd.Context(), "primary")
		if err != nil {
			return err
		}
		var match string
		for _, p := range pols {
			if p.Hash == args[1] || strings.HasPrefix(p.Hash, args[1]) {
				match = p.Hash
				break
			}
		}
		if match == "" {
			return fmt.Errorf("no policy matching %q", args[1])
		}
		if err := st.ApprovePolicy(cmd.Context(), match); err != nil {
			return err
		}
		// Move objective out of draft so scheduled/manual wakes may run.
		_ = st.SetObjectiveStatus(cmd.Context(), "primary", "active")
		_ = personal.WriteConfigMirror(cmd.Context(), home, st)
		fmt.Fprintf(cmd.OutOrStdout(), "Approved policy %s… for %s; objective is now active.\n", match[:12], args[0])
		return nil
	},
}

func init() { personalPolicyCmd.AddCommand(personalPolicySetCmd, personalPolicyShowCmd) }
