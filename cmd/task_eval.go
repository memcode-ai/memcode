package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/taskdetect"
)

// Every detection test uses a scripted classifier, which proves the pipeline
// and says nothing about the judgement. This runs the REAL classifier against
// cases with known answers and prints where it is wrong in each direction.
//
// A detector is only as good as its false-positive rate: offering to automate a
// one-off is worse than missing a recurring job, because the first teaches
// people to ignore the feature.

// evalCase is one labelled turn.
type evalCase struct {
	Request string
	Summary string
	// WantOffer is whether a well-behaved classifier should lead to an offer on
	// FIRST contact.
	WantOffer bool
	// WantKind is the recurrence kind expected, "" when several are defensible.
	WantKind taskdetect.RecurrenceKind
	Note     string
}

// evalCases spans the three things that matter: work that plainly recurs
// because the world moves, work that plainly does not, and work where a
// reasonable classifier could go either way.
var evalCases = []evalCase{
	// Inherently recurring — should be offered on first contact.
	{
		Request:   "Update our Anthropic model catalog — check for new and retired models and fix the ids.",
		Summary:   "Updated three model ids in catalog/models.json and corrected two prices. go test ./catalog/ passes.",
		WantOffer: true, WantKind: taskdetect.RecurrenceExternal,
		Note: "vendors change models independently of this repo",
	},
	{
		Request:   "Update our Go dependencies to their latest versions.",
		Summary:   "Bumped 6 modules in go.mod, ran go mod tidy, all tests pass.",
		WantOffer: true, WantKind: taskdetect.RecurrenceExternal,
		Note: "upstream packages keep releasing",
	},
	{
		Request:   "Check whether any of our dependencies have open security advisories.",
		Summary:   "Ran govulncheck; no advisories affect us today.",
		WantOffer: true, WantKind: taskdetect.RecurrenceExternal,
		Note: "advisories keep appearing",
	},
	{
		Request:   "Regenerate the weekly usage report from the metrics table.",
		Summary:   "Regenerated reports/usage.md from this week's rows.",
		WantOffer: true,
		Note:      "the underlying data keeps changing",
	},

	// One-offs — must NOT be offered, however automatable they look.
	{
		Request:   "Rename the company from Memcode AI to Foo everywhere in the repo.",
		Summary:   "Replaced 47 occurrences across 12 files; tests pass.",
		WantOffer: false, WantKind: taskdetect.RecurrenceOneOff,
		Note: "bounded, reproducible, evaluable — and renamed once",
	},
	{
		Request:   "Fix the nil pointer panic in internal/agent/runtime/loop.go line 412.",
		Summary:   "Added a nil guard and a regression test.",
		WantOffer: false, WantKind: taskdetect.RecurrenceOneOff,
		Note: "one specific bug",
	},
	{
		Request:   "Migrate the config file from JSON to YAML.",
		Summary:   "Converted the loader and the fixtures; tests pass.",
		WantOffer: false, WantKind: taskdetect.RecurrenceOneOff,
		Note: "a migration happens once",
	},
	{
		Request:   "Make this code better.",
		Summary:   "Tidied some naming in two files.",
		WantOffer: false,
		Note:      "unbounded and unevaluable — should fail the shape gates",
	},
	{
		Request:   "Why is the test failing? Walk me through it.",
		Summary:   "The fixture used an absolute path; explained and fixed.",
		WantOffer: false,
		Note:      "debugging one incident",
	},

	// Ambiguous — no assertion, printed for inspection.
	{
		Request: "Clean up the stale TODOs in this package.",
		Summary: "Removed 4 obsolete TODOs and filed 2 as issues.",
		Note:    "AMBIGUOUS: a user pattern at best, not inherently recurring",
	},
	{
		Request: "Check that our docs still match the CLI's actual flags.",
		Summary: "Found two stale flags in docs/cli.md and corrected them.",
		Note:    "AMBIGUOUS: recurs as the CLI changes — internal recurrence is defensible",
	},
}

var taskEvalCmd = &cobra.Command{
	Use:    "eval-detector",
	Short:  "internal: run the real task detector against labelled cases",
	Hidden: true,
	Long: `Runs the live classifier over cases with known answers and reports where it is wrong.

Costs one model call per case. Every other detection test uses a scripted classifier,
which proves the pipeline and says nothing about the judgement.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, prov, runner, err := openModelProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		_ = prov

		sess := runtime.New(st, runner, cfg.Root, "", permissions.ModeAsk, os.Stderr)
		classify := sess.TaskShapeClassifier()

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "VERDICT\tOFFER\tKIND\tCONF\tSHAPE\tREQUEST\tCAUSE")
		var falsePos, falseNeg, wrongKind int

		for _, c := range evalCases {
			p, ok, err := classify(ctx, taskdetect.Turn{
				SessionID: "eval", Project: cfg.Root,
				Request: c.Request, Summary: c.Summary,
			})
			if err != nil {
				fmt.Fprintf(w, "ERROR\t\t\t\t\t%s\t%v\n", clipTo(c.Request, 40), err)
				continue
			}
			// Mirror the detector's first-contact rule exactly, so the eval
			// measures what actually happens rather than a paraphrase of it.
			offered := ok && p.Eligible() && taskdetect.WouldOfferOnFirstContact(p)

			verdict := "ok"
			ambiguous := strings.HasPrefix(c.Note, "AMBIGUOUS")
			switch {
			case ambiguous:
				verdict = "info"
			case offered && !c.WantOffer:
				verdict, falsePos = "FALSE-POS", falsePos+1
			case !offered && c.WantOffer:
				verdict, falseNeg = "FALSE-NEG", falseNeg+1
			// Only judge the kind when a proposal was actually returned:
			// declining to propose at all is a correct outcome for a one-off,
			// not a mis-classified one.
			case ok && c.WantKind != "" && p.Recurrence.Kind != c.WantKind:
				verdict, wrongKind = "kind?", wrongKind+1
			}
			fmt.Fprintf(w, "%s\t%t\t%s\t%.2f\t%.2f\t%s\t%s\n",
				verdict, offered, p.Recurrence.Kind, p.Recurrence.Confidence, p.Confidence,
				clipTo(c.Request, 40), clipTo(p.Recurrence.Cause, 48))
		}
		w.Flush()

		fmt.Printf("\n%d false positive(s), %d false negative(s), %d questionable kind(s)\n",
			falsePos, falseNeg, wrongKind)
		if falsePos > 0 {
			fmt.Println("A false positive is the expensive one: offering to automate a one-off")
			fmt.Println("teaches people to ignore the offer.")
		}
		return nil
	},
}

func clipTo(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func init() { taskCmd.AddCommand(taskEvalCmd) }
