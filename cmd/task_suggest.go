package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/task"
	"github.com/memcode-ai/memcode/internal/taskdetect"
)

// Suggestions are surfaced rather than interrupted with.
//
// An offer that fires mid-turn would break the standing rule against injecting
// side concerns into work in progress, and an offer nobody can find later is an
// offer that only exists if you happen to be looking. So a recognised capability
// waits here, and at the start of the next session, where answering it is a
// choice rather than an interruption.

func openSignals(ctx context.Context) (*taskdetect.Store, error) {
	return taskdetect.OpenDefault(ctx)
}

// pending returns capabilities with enough evidence to be worth offering.
func pending(ctx context.Context, s *taskdetect.Store, project string, now time.Time) ([]taskdetect.Cluster, error) {
	clusters, err := s.Clusters(ctx, project, now)
	if err != nil {
		return nil, err
	}
	var out []taskdetect.Cluster
	for _, c := range clusters {
		if !taskdetect.Ready(c) {
			continue
		}
		if ok, err := s.Suppressed(ctx, c.Latest); err == nil && ok {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

var taskSuggestCmd = &cobra.Command{
	Use:     "suggestions",
	Aliases: []string{"suggest"},
	Short:   "Work memcode has noticed you repeat, and could take over",
	Long: `memcode watches what it does for you and notices when a piece of work is a standing
job rather than a one-off — bounded, reproducible, safe to run unattended, and checkable.

A capability appears here once it has come up enough times, across enough separate sessions,
to be a habit rather than a coincidence. Accepting one writes a complete task; nothing is
created without you saying so.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		s, err := openSignals(ctx)
		if err != nil {
			return err
		}
		defer s.Close()

		root := taskRoot()
		ready, err := pending(ctx, s, root, time.Now())
		if err != nil {
			return err
		}
		if len(ready) == 0 {
			fmt.Println("Nothing to suggest yet. memcode proposes work once it has seen the same")
			fmt.Println("kind of job come up across a few separate sessions.")
			return nil
		}
		for _, c := range ready {
			d := taskdetect.Decision{
				Kind: taskdetect.KindRepeated, Proposal: c.Latest,
				Sessions: c.Sessions, Occasions: c.Signals,
			}
			msg := taskdetect.Message(d)
			fmt.Printf("\n%s\n", msg.Headline)
			fmt.Printf("  %s\n", msg.Detail)
			fmt.Printf("  family   %s\n", c.Family)
			for _, e := range c.Evidence {
				fmt.Printf("  seen     %s\n", e)
			}
			fmt.Printf("\n  accept   memcode task suggestions create %s\n", c.Family)
			fmt.Printf("  refuse   memcode task suggestions never %s\n", c.Family)
		}
		return nil
	},
}

var taskSuggestCreateCmd = &cobra.Command{
	Use:   "create <family>",
	Short: "Accept a suggestion and write the task",
	Long: `Writes the proposed task exactly as offered: cadence, authority, verification and
runtime policy are all inferred, so accepting is one step rather than a questionnaire.

Edit the resulting file if you want something different — it is ordinary YAML.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		s, err := openSignals(ctx)
		if err != nil {
			return err
		}
		defer s.Close()

		root := taskRoot()
		c, err := findCluster(ctx, s, root, args[0])
		if err != nil {
			return err
		}
		// Accepting a cluster accepts the CAPABILITY, so the task is named for it.
		t, err := c.Capability().ToTask(time.Now())
		if err != nil {
			return err
		}
		scope := task.ScopeProject
		if root == "" {
			scope = task.ScopeGlobal
		}
		path, err := task.Save(root, t, scope)
		if err != nil {
			return err
		}
		// The task now exists, so its evidence has done its job. Leaving it
		// would keep proposing something that already runs.
		if err := s.Forget(ctx, c.Latest); err != nil {
			return err
		}
		fmt.Printf("Created %s\n\n", path)
		fmt.Printf("  %s\n", t.Description)
		fmt.Printf("  %s · %s\n", cadence(t), t.Autonomy.Level)
		fmt.Printf("\nRun it now with `memcode task run %s`, or leave it to its schedule.\n", t.Name)
		warnIfShadowed(root)
		return nil
	},
}

var taskSuggestNeverCmd = &cobra.Command{
	Use:     "never <family>",
	Aliases: []string{"dismiss"},
	Short:   "Stop suggesting this kind of task",
	Long: `Durably refuses a KIND of work, not one phrasing of it. Declining provider-catalog
maintenance says nothing about a weekly dependency audit, and the refusal survives the
classifier naming the same capability slightly differently next time.

Lift it again with ` + "`memcode task suggestions allow <family>`" + `.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		s, err := openSignals(ctx)
		if err != nil {
			return err
		}
		defer s.Close()
		c, err := findCluster(ctx, s, taskRoot(), args[0])
		if err != nil {
			return err
		}
		if err := s.Suppress(ctx, c.Latest, time.Now()); err != nil {
			return err
		}
		if err := s.Forget(ctx, c.Latest); err != nil {
			return err
		}
		fmt.Printf("Won't suggest %s again.\n", c.Family)
		return nil
	},
}

var taskSuggestAllowCmd = &cobra.Command{
	Use:   "allow <family>",
	Short: "Lift a previous refusal",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		s, err := openSignals(ctx)
		if err != nil {
			return err
		}
		defer s.Close()
		n, err := s.Unsuppress(ctx, args[0])
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no refusal recorded for %q", args[0])
		}
		fmt.Printf("Will suggest %s again when the evidence supports it.\n", args[0])
		return nil
	},
}

var taskSuggestRefusedCmd = &cobra.Command{
	Use:   "refused",
	Short: "Show kinds of task you have declined",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		s, err := openSignals(ctx)
		if err != nil {
			return err
		}
		defer s.Close()
		list, err := s.Suppressions(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("Nothing refused.")
			return nil
		}
		for _, l := range list {
			fmt.Printf("  %s\n", l)
		}
		return nil
	},
}

// findCluster resolves a family name to its accumulated evidence, accepting a
// prefix so nobody has to retype a long identifier exactly.
func findCluster(ctx context.Context, s *taskdetect.Store, project, family string) (taskdetect.Cluster, error) {
	clusters, err := s.Clusters(ctx, project, time.Now())
	if err != nil {
		return taskdetect.Cluster{}, err
	}
	var matches []taskdetect.Cluster
	for _, c := range clusters {
		if c.Family == family || strings.HasPrefix(c.Family, family) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return taskdetect.Cluster{}, fmt.Errorf("no suggestion matching %q — see `memcode task suggestions`", family)
	case 1:
		return matches[0], nil
	default:
		var names []string
		for _, m := range matches {
			names = append(names, m.Family)
		}
		return taskdetect.Cluster{}, fmt.Errorf("%q matches several: %s", family, strings.Join(names, ", "))
	}
}

func init() {
	taskSuggestCmd.AddCommand(taskSuggestCreateCmd, taskSuggestNeverCmd,
		taskSuggestAllowCmd, taskSuggestRefusedCmd)
	taskCmd.AddCommand(taskSuggestCmd)
}
