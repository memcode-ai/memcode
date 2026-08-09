package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/store"
)

var claimsCmd = &cobra.Command{
	Use:   "claims",
	Short: "Inspect adjudicated claims (what currently governs the repo)",
}

var claimsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List current claims (and candidates), grouped by status",
	RunE: func(cmd *cobra.Command, args []string) error {
		return withClaims(cmd, func(claims []store.Claim) error {
			printClaimGroup("Current claims", claims, "current")
			printClaimGroup("Candidates (uncorroborated)", claims, "candidate")
			n := countStatus(claims, "conflicted") + countStatus(claims, "stale")
			if n > 0 {
				fmt.Printf("\n%d conflicted/stale claim(s) — see `memcode claims conflicts`.\n", n)
			}
			return nil
		})
	},
}

var claimsConflictsCmd = &cobra.Command{
	Use:   "conflicts",
	Short: "Show conflicted and stale claims (do not treat these as current)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return withClaims(cmd, func(claims []store.Claim) error {
			printClaimGroup("Conflicted", claims, "conflicted")
			printClaimGroup("Stale", claims, "stale")
			if countStatus(claims, "conflicted")+countStatus(claims, "stale") == 0 {
				fmt.Println("No conflicted or stale claims. 🎉")
			}
			return nil
		})
	},
}

func withClaims(cmd *cobra.Command, fn func([]store.Claim) error) error {
	ctx := cmd.Context()
	st, _, err := openProject(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	claims, err := st.ListClaims(ctx)
	if err != nil {
		return err
	}
	if len(claims) == 0 {
		fmt.Println("No claims yet. Run `memcode learn`.")
		return nil
	}
	return fn(claims)
}

func printClaimGroup(title string, claims []store.Claim, status string) {
	var group []store.Claim
	for _, c := range claims {
		if c.Status == status {
			group = append(group, c)
		}
	}
	if len(group) == 0 {
		return
	}
	fmt.Printf("%s:\n", title)
	for _, c := range group {
		scope := c.Scope
		if scope == "" || scope == "." {
			scope = "repo"
		}
		fmt.Printf("  - %s\n", c.Text)
		fmt.Printf("      [%s · %s · %s] source: %s · %s\n",
			c.Type, scope, orDash(c.Confidence), orDash(c.SourcePath), orDash(c.Evidence))
	}
	fmt.Println()
}

func countStatus(claims []store.Claim, status string) int {
	n := 0
	for _, c := range claims {
		if c.Status == status {
			n++
		}
	}
	return n
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	claimsCmd.AddCommand(claimsListCmd, claimsConflictsCmd)
	rootCmd.AddCommand(claimsCmd)
}
