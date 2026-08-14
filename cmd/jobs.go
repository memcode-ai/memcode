package cmd

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/jobs"
)

var jobsCmd = &cobra.Command{
	Use:   "jobs",
	Short: "List and inspect background agent jobs",
	Long: `Background agent sessions started with "memcode run <task> --background"
run as detached child processes that coordinate through a single repo-wide
writer lock (one writer at a time). This command lists them and shows their logs.`,
	RunE: func(cmd *cobra.Command, args []string) error { return runJobsList(cmd) },
}

var jobsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List background jobs (newest first)",
	RunE:  func(cmd *cobra.Command, args []string) error { return runJobsList(cmd) },
}

var jobsLogsCmd = &cobra.Command{
	Use:   "logs <id>",
	Short: "Print a job's output log",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, cfg, err := openProject(cmd.Context())
		if err != nil {
			return err
		}
		f, err := os.Open(jobs.LogPath(cfg.Root, args[0]))
		if err != nil {
			return fmt.Errorf("no log for job %s", args[0])
		}
		defer f.Close()
		_, err = io.Copy(os.Stdout, f)
		return err
	},
}

var jobsStopCmd = &cobra.Command{
	Use:   "stop <id>",
	Short: "Stop a running background agent job",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, cfg, err := openProject(cmd.Context())
		if err != nil {
			return err
		}
		if err := jobs.Stop(cfg.Root, args[0]); err != nil {
			return err
		}
		fmt.Printf("stopped job %s\n", args[0])
		return nil
	},
}

func runJobsList(cmd *cobra.Command) error {
	_, cfg, err := openProject(cmd.Context())
	if err != nil {
		return err
	}
	list, err := jobs.List(cfg.Root)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no background jobs yet — start one with `memcode run \"<task>\" --background`")
		return nil
	}
	for _, j := range list {
		when := j.StartedAt.Local().Format("Jan 2 15:04")
		dur := ""
		if !j.FinishedAt.IsZero() {
			dur = " · " + j.FinishedAt.Sub(j.StartedAt).Round(time.Second).String()
		}
		fmt.Printf("%s  %-8s  %s%s\n    %s\n", j.ID, j.Status, when, dur, j.Task)
	}
	return nil
}

func init() {
	jobsCmd.AddCommand(jobsListCmd, jobsLogsCmd, jobsStopCmd)
	rootCmd.AddCommand(jobsCmd)
}
