package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/gitlog"
	"github.com/memcode-ai/memcode/internal/producer"
	"github.com/memcode-ai/memcode/internal/provenance"
)

var whyCmd = &cobra.Command{
	Use:   "why <path|subsystem>",
	Short: "Explain why something exists — its provenance",
	Long: `Answers "why is this here?" for a file, directory or subsystem: when and
by whom it was introduced, how it has evolved, which subsystem it belongs to,
what it depends on, the objectives it serves, the constraints that govern it, and
the tests that cover it. Deterministic — drawn from git history, topology,
objectives and doctrine. No model.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		asJSON, _ := cmd.Flags().GetBool("json")
		target := strings.Join(args, " ")

		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		p, err := provenance.Why(ctx, st, cfg.Root, target)
		if err != nil {
			return err
		}

		if asJSON {
			b, err := json.MarshalIndent(p, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}
		renderProvenance(p)
		return nil
	},
}

func renderProvenance(p provenance.Provenance) {
	fmt.Printf("why: %s", p.Target)
	if p.Subsystem != "" && p.Subsystem != p.Target {
		fmt.Printf("   (part of: %s)", p.Subsystem)
	}
	fmt.Println()

	if p.Introduced != nil {
		c := p.Introduced
		fmt.Printf("\nintroduced:  %s  by %s%s  on %s\n", c.Hash, c.Author, producerTag(*c), c.Date)
		fmt.Printf("             %s\n", c.Subject)
	}
	if len(p.Evolved) > 0 {
		fmt.Printf("\nevolved:\n")
		for _, c := range p.Evolved {
			fmt.Printf("  %s  %s  %s%s  %s\n", c.Hash, c.Date, c.Author, producerTag(c), c.Subject)
		}
	}
	if len(p.DependsOn) > 0 {
		fmt.Printf("\ndepends on:     %s\n", strings.Join(p.DependsOn, ", "))
	}
	if len(p.DependedBy) > 0 {
		fmt.Printf("depended on by: %s\n", strings.Join(p.DependedBy, ", "))
	}
	if len(p.Serves) > 0 {
		fmt.Printf("\nserves:\n")
		for _, g := range p.Serves {
			fmt.Printf("  [%s] %s\n", g.Status, g.Title)
		}
	}
	if len(p.ConstrainedBy) > 0 {
		fmt.Printf("\nconstrained by:\n")
		for _, c := range p.ConstrainedBy {
			fmt.Printf("  %s\n", c)
		}
	}
	if len(p.TestedBy) > 0 {
		fmt.Printf("\ntested by:\n")
		for _, t := range p.TestedBy {
			fmt.Printf("  %s\n", t)
		}
	}
	if len(p.Notes) > 0 {
		fmt.Printf("\nnotes:\n")
		for _, n := range p.Notes {
			fmt.Printf("  · %s\n", n)
		}
	}
}

// producerTag annotates a commit with its AI-tool producer (blank for humans).
func producerTag(c gitlog.Commit) string {
	r := producer.Classify(c.Author, c.AuthorEmail, c.Subject, c.Body)
	if r.Producer == producer.Human {
		return ""
	}
	return "  [" + string(r.Producer) + "]"
}

func init() {
	whyCmd.Flags().Bool("json", false, "output raw provenance as JSON")
	rootCmd.AddCommand(whyCmd)
}
