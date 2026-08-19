package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/gateway/state"
)

// gatewayScheduleCmd manages the gateway's time-triggered tasks from the
// terminal — the deterministic counterpart to asking `memcode admin` in chat.
// The verb set matches what OpenClaw and Hermes users already know (add, list,
// show, edit, run, enable/disable, remove), with their aliases accepted. Edits
// land in gateway.yaml; a running gateway picks them up within seconds, no
// restart.
var gatewayScheduleCmd = &cobra.Command{
	Use:     "schedule",
	Aliases: []string{"cron", "automations"}, // what OpenClaw/Hermes call this — honor migrating muscle memory
	Short:   "Manage scheduled tasks (add, list, show, edit, run, enable, disable, remove)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return scheduleList(cmd)
	},
}

var gatewayScheduleListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List configured schedules",
	RunE: func(cmd *cobra.Command, args []string) error {
		return scheduleList(cmd)
	},
}

var (
	scheduleCron  string
	scheduleEvery string
	scheduleAt    string
	scheduleTo    string
	scheduleTZ    string
	scheduleAgent string
)

// validateSpec checks that exactly one schedule form is set and that it parses
// — the SAME gwconfig validation the admin tools run, so the two surfaces
// cannot drift. Returns the resolved at timestamp ("" unless --at was used).
func validateSpec() (at string, err error) {
	return gwconfig.ValidateScheduleSpec(scheduleCron, scheduleEvery, scheduleAt, time.Now())
}

var gatewayScheduleAddCmd = &cobra.Command{
	Use:     "add <name> <task...>",
	Aliases: []string{"create"},
	Short:   "Add a scheduled task",
	Long: `Add a scheduled task. Set exactly one of --cron, --every, or --at, and --to
for where the result is delivered.

Examples:
  memcode gateway schedule add standup --cron "0 9 * * 1-5" --to telegram:123456 "Summarize yesterday's commits and open PRs"
  memcode gateway schedule add health --every 30m --to slack:C0123 "Check disk, memory, and the error log; report only if something is off"
  memcode gateway schedule add remind --at 3h --to telegram:123456 "Remind me to review the release notes"`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := gwconfig.BuildSchedule(args[0], scheduleCron, scheduleEvery, scheduleAt,
			scheduleTZ, strings.Join(args[1:], " "), scheduleTo, scheduleAgent, time.Now())
		if err != nil {
			return err
		}
		settings, err := gwconfig.Load()
		if err != nil {
			return err
		}
		if err := settings.AddSchedule(sc); err != nil {
			return err
		}
		if err := gwconfig.Save(settings); err != nil {
			return err
		}
		if sc.At != "" {
			cmd.Printf("Scheduled one-shot %s at %s → %s. A running gateway picks this up within a few seconds.\n", sc.Name, sc.At, sc.DeliverTo)
		} else {
			cmd.Printf("Scheduled %s → %s. A running gateway picks this up within a few seconds.\n", sc.Name, sc.DeliverTo)
		}
		return nil
	},
}

var gatewayScheduleShowCmd = &cobra.Command{
	Use:     "show <name>",
	Aliases: []string{"get"},
	Short:   "Show one schedule in full",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, _, err := findSchedule(args[0])
		if err != nil {
			return err
		}
		cmd.Printf("name:       %s\n", sc.Name)
		switch {
		case sc.Cron != "":
			cmd.Printf("cron:       %s\n", sc.Cron)
		case sc.Every != "":
			cmd.Printf("every:      %s\n", sc.Every)
		case sc.At != "":
			cmd.Printf("at:         %s (one-shot)\n", sc.At)
		}
		cmd.Printf("deliver_to: %s\n", sc.DeliverTo)
		if sc.Agent != "" {
			cmd.Printf("agent:      %s\n", sc.Agent)
		}
		if sc.TZ != "" {
			cmd.Printf("tz:         %s\n", sc.TZ)
		}
		cmd.Printf("task:       %s\n", sc.Task)
		if sc.Disabled {
			cmd.Println("state:      disabled")
		}
		return nil
	},
}

var gatewayScheduleEditCmd = &cobra.Command{
	Use:   "edit <name> [new task...]",
	Short: "Update a schedule's timing, delivery, or task",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(args[0])
		settings, err := gwconfig.Load()
		if err != nil {
			return err
		}
		for i, sc := range settings.Schedules {
			if sc.Name != name {
				continue
			}
			if scheduleCron != "" || scheduleEvery != "" || scheduleAt != "" {
				at, err := validateSpec()
				if err != nil {
					return err
				}
				sc.Cron, sc.Every, sc.At = scheduleCron, scheduleEvery, at
			}
			if scheduleTo != "" {
				if err := gwconfig.ValidateDeliverTo(scheduleTo); err != nil {
					return err
				}
				sc.DeliverTo = scheduleTo
			}
			// --tz/--agent: an untouched flag leaves the field unchanged; passing
			// the flag — including an explicit "" — sets (or clears) it.
			if cmd.Flags().Changed("tz") {
				sc.TZ = strings.TrimSpace(scheduleTZ)
			}
			if cmd.Flags().Changed("agent") {
				sc.Agent = strings.TrimSpace(scheduleAgent)
			}
			if len(args) > 1 {
				sc.Task = strings.TrimSpace(strings.Join(args[1:], " "))
			}
			settings.Schedules[i] = sc
			if err := gwconfig.Save(settings); err != nil {
				return err
			}
			cmd.Printf("Updated schedule %s.\n", name)
			return nil
		}
		return fmt.Errorf("no schedule %q", name)
	},
}

// scheduleRunTarget resolves the agent and project a run-now task snapshots,
// mirroring the gateway's own Deliver-time resolution: the conversation's
// explicit choice, else the channel/gateway defaults; the schedule's pinned
// agent overrides (a Trusted fire carries it); the project always satisfies
// the channel's project policy.
func scheduleRunTarget(settings gwconfig.Settings, sc gwconfig.Schedule, channel, convAgent, convProject string) (agent, project string) {
	agent = settings.Get(channel).Agent
	project = settings.DefaultProject
	if convAgent != "" {
		agent = convAgent
	}
	if convProject != "" {
		project = convProject
	}
	if sc.Agent != "" {
		agent = sc.Agent
	}
	if !settings.ProjectAllowed(channel, project) {
		if ps := settings.Get(channel).Projects; len(ps) > 0 {
			project = ps[0]
		}
	}
	return agent, project
}

var gatewayScheduleRunCmd = &cobra.Command{
	Use:   "run <name>",
	Short: "Run a schedule's task now, without waiting for its next fire",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, settings, err := findSchedule(args[0])
		if err != nil {
			return err
		}
		ch, convo, ok := strings.Cut(sc.DeliverTo, ":")
		if !ok {
			return fmt.Errorf("schedule %q has a bad deliver_to %q", sc.Name, sc.DeliverTo)
		}
		gw, err := openGatewayState(cmd.Context())
		if err != nil {
			return err
		}
		defer gw.Close()
		// Snapshot the SAME selection a timed fire gets from Deliver — the
		// conversation's chosen agent/project, else channel/gateway defaults,
		// with the schedule's own agent pin winning — so run-now never silently
		// downgrades to the gateway defaults.
		convAgent, convProject, _ := gw.Conversation(cmd.Context(), ch, convo)
		agent, project := scheduleRunTarget(settings, sc, ch, convAgent, convProject)
		// Enqueue the same Trusted item a timed fire would; the running gateway's
		// worker drains it within seconds.
		_, err = gw.Accept(cmd.Context(), state.Item{
			Channel: ch, Conversation: convo,
			MessageID: fmt.Sprintf("cli:%s:%d", sc.Name, time.Now().UnixNano()),
			Principal: "schedule:" + sc.Name, Text: sc.Task, Trusted: true,
			Agent: agent, Project: project,
		}, time.Now())
		if err != nil {
			return err
		}
		cmd.Printf("Queued %s — the gateway runs it now and delivers to %s.\n", sc.Name, sc.DeliverTo)
		return nil
	},
}

var gatewayScheduleEnableCmd = &cobra.Command{
	Use:     "enable <name>",
	Aliases: []string{"resume"},
	Short:   "Re-enable a disabled schedule",
	Args:    cobra.ExactArgs(1),
	RunE:    func(cmd *cobra.Command, args []string) error { return setScheduleDisabled(cmd, args[0], false) },
}

var gatewayScheduleDisableCmd = &cobra.Command{
	Use:     "disable <name>",
	Aliases: []string{"pause"},
	Short:   "Pause a schedule without deleting it",
	Args:    cobra.ExactArgs(1),
	RunE:    func(cmd *cobra.Command, args []string) error { return setScheduleDisabled(cmd, args[0], true) },
}

var gatewayScheduleRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove a scheduled task",
	Args:    cobra.ExactArgs(1),
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

func findSchedule(name string) (gwconfig.Schedule, gwconfig.Settings, error) {
	name = strings.TrimSpace(name)
	settings, err := gwconfig.Load()
	if err != nil {
		return gwconfig.Schedule{}, settings, err
	}
	for _, sc := range settings.Schedules {
		if sc.Name == name {
			return sc, settings, nil
		}
	}
	return gwconfig.Schedule{}, settings, fmt.Errorf("no schedule %q", name)
}

func setScheduleDisabled(cmd *cobra.Command, name string, disabled bool) error {
	name = strings.TrimSpace(name)
	settings, err := gwconfig.Load()
	if err != nil {
		return err
	}
	for i, sc := range settings.Schedules {
		if sc.Name != name {
			continue
		}
		sc.Disabled = disabled
		settings.Schedules[i] = sc
		if err := gwconfig.Save(settings); err != nil {
			return err
		}
		if disabled {
			cmd.Printf("Disabled schedule %s — it stays configured and can be re-enabled.\n", name)
		} else {
			cmd.Printf("Enabled schedule %s.\n", name)
		}
		return nil
	}
	return fmt.Errorf("no schedule %q", name)
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
		if sc.At != "" {
			when = "at " + sc.At
		}
		flag := ""
		if sc.Disabled {
			flag = " (disabled)"
		}
		cmd.Printf("%-16s %-24s → %-24s %s%s\n", sc.Name, when, sc.DeliverTo, sc.Task, flag)
	}
	return nil
}

// scheduleSpecFlags attaches the timing/delivery flags shared by add and edit.
func scheduleSpecFlags(c *cobra.Command) {
	c.Flags().StringVar(&scheduleCron, "cron", "", "5-field cron expression, e.g. \"0 9 * * 1-5\"")
	c.Flags().StringVar(&scheduleEvery, "every", "", "interval as a Go duration, e.g. 30m or 24h")
	c.Flags().StringVar(&scheduleAt, "at", "", "one-shot: a duration from now (30m) or a date-time (2026-03-01T09:00)")
	c.Flags().StringVar(&scheduleTo, "to", "", "where the result is delivered: \"channel:conversation\"")
	c.Flags().StringVar(&scheduleTZ, "tz", "", "evaluate --cron in this zone, e.g. America/Los_Angeles (default: local; on edit, --tz \"\" clears it)")
	c.Flags().StringVar(&scheduleAgent, "agent", "", "run as this agent (its pinned model and instructions apply; on edit, --agent \"\" clears it)")
}

func init() {
	scheduleSpecFlags(gatewayScheduleAddCmd)
	_ = gatewayScheduleAddCmd.MarkFlagRequired("to")
	scheduleSpecFlags(gatewayScheduleEditCmd)
	gatewayScheduleCmd.AddCommand(
		gatewayScheduleListCmd, gatewayScheduleAddCmd, gatewayScheduleShowCmd,
		gatewayScheduleEditCmd, gatewayScheduleRunCmd,
		gatewayScheduleEnableCmd, gatewayScheduleDisableCmd, gatewayScheduleRemoveCmd,
	)
	gatewayCmd.AddCommand(gatewayScheduleCmd)
}
