package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/objectives"
)

var objectiveCmd = &cobra.Command{
	Use:     "objective",
	Aliases: []string{"obj"},
	Short:   "Manage the goals you're working toward (human-authored)",
	Long: `Objectives are the goals that give memcode its sense of direction —
what you're trying to do, not just what happened. They are authored by you; the
system never invents them.`,
}

var objectiveAddCmd = &cobra.Command{
	Use:   "add <title>",
	Short: "Record a new objective",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		priority, _ := cmd.Flags().GetInt("priority")
		parent, _ := cmd.Flags().GetString("parent")
		title := strings.Join(args, " ")

		st, _, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		o, err := objectives.New(st).Add(ctx, title, priority, parent)
		if err != nil {
			return err
		}
		fmt.Printf("Added objective %s: %s\n", o.ID, o.Title)
		return nil
	},
}

var objectiveListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all objectives",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, _, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		all, err := objectives.New(st).List(ctx)
		if err != nil {
			return err
		}
		printObjectives(all, "No objectives yet. Add one with `memcode objective add`.")
		return nil
	},
}

var objectiveCurrentCmd = &cobra.Command{
	Use:   "current",
	Short: "Show the objectives currently in flight",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, _, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		cur, err := objectives.New(st).Current(ctx)
		if err != nil {
			return err
		}
		printObjectives(cur, "Nothing in flight. Add an objective with `memcode objective add`.")
		return nil
	},
}

var objectiveStatusCmd = &cobra.Command{
	Use:   "status <id> <status>",
	Short: "Set an objective's status (proposed|active|blocked|done|abandoned)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, _, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		o, err := objectives.New(st).SetStatus(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		fmt.Printf("%s is now %s\n", o.ID, o.Status)
		return nil
	},
}

func printObjectives(list []objectives.Objective, empty string) {
	if len(list) == 0 {
		fmt.Println(empty)
		return
	}
	for _, o := range list {
		fmt.Printf("  %s  [%-8s] %s\n", o.ID, o.Status, o.Title)
	}
}

func init() {
	objectiveAddCmd.Flags().IntP("priority", "p", 0, "priority (higher sorts first)")
	objectiveAddCmd.Flags().String("parent", "", "parent objective id")
	objectiveCmd.AddCommand(objectiveAddCmd, objectiveListCmd, objectiveCurrentCmd, objectiveStatusCmd)
	rootCmd.AddCommand(objectiveCmd)
}
