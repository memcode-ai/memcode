package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// gatewayScheduleCmd manages the gateway's time-triggered tasks from the
// terminal — the deterministic counterpart to asking `memcode admin` in chat.
// Edits land in gateway.yaml; a running gateway picks them up within seconds,
// no restart.
var gatewayScheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Manage scheduled tasks (list, add, remove)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return scheduleList(cmd)
	},
}

var gatewayScheduleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured schedules",
	RunE: func(cmd *cobra.Command, args []string) error {
		return scheduleList(cmd)
	},
}

var (
	scheduleCron  string
	scheduleEvery string
	scheduleTo    string
)

var gatewayScheduleAddCmd = &cobra.Command{
	Use:   "add <name> <task...>",
	Short: "Add a scheduled task",
	Long: `Add a scheduled task. Set exactly one of --cron or --every, and --to for
where the result is delivered.

Examples:
  memcode gateway schedule add standup --cron "0 9 * * 1-5" --to telegram:123456 "Summarize yesterday's commits and open PRs"
  memcode gateway schedule add health --every 30m --to slack:C0123 "Check disk, memory, and the error log; report only if something is off"`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(args[0])
		task := strings.TrimSpace(strings.Join(args[1:], " "))
		if name == "" || task == "" {
			return fmt.Errorf("a schedule needs a name and a task")
		}
		switch {
		case scheduleCron != "" && scheduleEvery != "":
			return fmt.Errorf("set exactly one of --cron or --every, not both")
		case scheduleCron != "":
			if _, err := cron.ParseStandard(scheduleCron); err != nil {
				return fmt.Errorf("bad --cron %q: %w (5 fields: minute hour day-of-month month day-of-week)", scheduleCron, err)
			}
		case scheduleEvery != "":
			if _, err := time.ParseDuration(scheduleEvery); err != nil {
				return fmt.Errorf("bad --every %q: %w (a Go duration like 30m or 24h)", scheduleEvery, err)
			}
		default:
			return fmt.Errorf("set --cron (e.g. \"0 9 * * 1-5\") or --every (e.g. 24h)")
		}
		to := strings.TrimSpace(scheduleTo)
		if ch, convo, ok := strings.Cut(to, ":"); !ok || ch == "" || convo == "" {
			return fmt.Errorf("--to must be \"channel:conversation\", e.g. telegram:123456")
		}
		settings, err := gwconfig.Load()
		if err != nil {
			return err
		}
		for _, sc := range settings.Schedules {
			if sc.Name == name {
				return fmt.Errorf("schedule %q already exists — remove it first to replace it", name)
			}
		}
		settings.Schedules = append(settings.Schedules, gwconfig.Schedule{
			Name: name, Cron: scheduleCron, Every: scheduleEvery, Task: task, DeliverTo: to,
		})
		if err := gwconfig.Save(settings); err != nil {
			return err
		}
		cmd.Printf("Scheduled %s → %s. A running gateway picks this up within a few seconds.\n", name, to)
		return nil
	},
}

var gatewayScheduleRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a scheduled task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(args[0])
		settings, err := gwconfig.Load()
		if err != nil {
			return err
		}
		kept := settings.Schedules[:0]
		found := false
		for _, sc := range settings.Schedules {
			if sc.Name == name {
				found = true
				continue
			}
			kept = append(kept, sc)
		}
		if !found {
			return fmt.Errorf("no schedule %q", name)
		}
		settings.Schedules = kept
		if err := gwconfig.Save(settings); err != nil {
			return err
		}
		cmd.Printf("Removed schedule %s.\n", name)
		return nil
	},
}

func scheduleList(cmd *cobra.Command) error {
	settings, err := gwconfig.Load()
	if err != nil {
		return err
	}
	if len(settings.Schedules) == 0 {
		cmd.Println("No schedules. Add one with `memcode gateway schedule add`.")
		return nil
	}
	for _, sc := range settings.Schedules {
		when := sc.Cron
		if sc.Every != "" {
			when = "every " + sc.Every
		}
		cmd.Printf("%-16s %-16s → %-24s %s\n", sc.Name, when, sc.DeliverTo, sc.Task)
	}
	return nil
}

func init() {
	gatewayScheduleAddCmd.Flags().StringVar(&scheduleCron, "cron", "", "5-field cron expression, e.g. \"0 9 * * 1-5\"")
	gatewayScheduleAddCmd.Flags().StringVar(&scheduleEvery, "every", "", "interval as a Go duration, e.g. 30m or 24h")
	gatewayScheduleAddCmd.Flags().StringVar(&scheduleTo, "to", "", "where the result is delivered: \"channel:conversation\"")
	_ = gatewayScheduleAddCmd.MarkFlagRequired("to")
	gatewayScheduleCmd.AddCommand(gatewayScheduleListCmd, gatewayScheduleAddCmd, gatewayScheduleRemoveCmd)
	gatewayCmd.AddCommand(gatewayScheduleCmd)
}
