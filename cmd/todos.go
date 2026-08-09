package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/todos"
)

var todosCmd = &cobra.Command{
	Use:   "todos",
	Short: "Show the agent's most recent work-tracker checklist",
	Long: `Todos are the agent's own execution scratchpad — a lightweight checklist it
keeps so it doesn't lose track of multi-step work. They live in the running
session's memory; each change is snapshotted to the event log, and this command
shows the latest snapshot. Todos are NOT durable project goals: those are
objectives (` + "`memcode objective`" + `), and deliberate planning is a separate mode.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, _, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		list, err := todos.Current(ctx, st)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("No todos recorded yet — the agent creates a checklist when a task has several steps.")
			return nil
		}
		fmt.Printf("todos  %s\n%s\n", list.Summary(), list.Render("  "))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(todosCmd)
}
