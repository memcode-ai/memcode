package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/sessionprune"
	"github.com/memcode-ai/memcode/internal/store"
)

var sessionDeleteCmd = &cobra.Command{
	Use:   "delete <session-id>... | --all",
	Short: "Delete recorded sessions from this machine",
	Long: `Delete recorded sessions from this machine.

Removes everything memcode kept locally for the session: its conversation
(what /resume and --continue re-enter), its activity log, its rewind
checkpoints, its wire trace, and its entries in session history and recall.
A session id may be shortened to any unique prefix ("sess_" is optional).

What the session taught memcode stays: memory, learned preferences and
lessons, and the project's settings are not touched.

A session that is open in a running memcode is refused; quit it first.
Deleting a session that is already gone succeeds and does nothing.

  memcode session delete sess_3f9a1c2e
  memcode session delete 3f9a
  memcode session delete --all          # every session of this project (asks first)
  memcode session delete --all --yes    # without asking`,
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		yes, _ := cmd.Flags().GetBool("yes")
		if all == (len(args) > 0) {
			return fmt.Errorf("name the session(s) to delete, or pass --all")
		}
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		sessions, err := sessionprune.List(ctx, cfg.Root, st)
		if err != nil {
			return err
		}

		var ids []string
		if all {
			live := 0
			for _, s := range sessions {
				if s.Live {
					live++
					continue
				}
				ids = append(ids, s.ID)
			}
			if len(ids) == 0 {
				fmt.Println("No sessions to delete.")
				return nil
			}
			if !yes {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("refusing to delete every session without confirmation — pass --yes")
				}
				note := ""
				if live > 0 {
					note = fmt.Sprintf(" (%d open session(s) will be kept)", live)
				}
				fmt.Printf("Delete all %d session(s) of %s%s? This cannot be undone. [y/N] ", len(ids), cfg.Root, note)
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
					fmt.Println("Cancelled.")
					return nil
				}
			}
		} else {
			for _, ref := range args {
				id, err := sessionprune.Resolve(sessions, ref)
				if errors.Is(err, sessionprune.ErrNotFound) {
					fmt.Printf("Nothing to delete: no session matches %q.\n", ref)
					continue
				}
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			if len(ids) == 0 {
				return nil
			}
		}

		res, err := sessionprune.Delete(ctx, cfg.Root, st, ids...)
		for _, id := range res.Sessions {
			fmt.Printf("Deleted %s\n", id)
		}
		if errors.Is(err, sessionprune.ErrLive) {
			return fmt.Errorf("%w — quit that memcode first", err)
		}
		return err
	},
}

var sessionPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Delete sessions idle longer than the retention period, now",
	Long: `Delete every session whose last activity is older than the retention
period (see "memcode session retention"). memcode already does this on its
own once a day when it starts; this runs it right away.

  memcode session prune             # use the configured retention
  memcode session prune --days 7    # this once, keep only the last week`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()
		days, _ := config.SessionRetention(cfg)
		if cmd.Flags().Changed("days") {
			days, _ = cmd.Flags().GetInt("days")
		}
		if days <= 0 {
			fmt.Println("Retention is set to keep sessions forever; nothing pruned.")
			return nil
		}
		res, err := sessionprune.Prune(ctx, cfg.Root, st, sessionprune.Options{RetentionDays: days})
		if err != nil {
			return err
		}
		fmt.Printf("Deleted %d session(s) idle for more than %d day(s).\n", len(res.Sessions), days)
		return nil
	},
}

var sessionRetentionCmd = &cobra.Command{
	Use:   "retention [days|default]",
	Short: "Show or set how many days sessions are kept (0 = forever)",
	Long: `Show or set how long memcode keeps a session after its last activity.
Older sessions are deleted automatically, once a day, when memcode starts.
The session you are resuming, and any session open in a running memcode,
are never deleted. 0 keeps sessions forever. The default is 30 days.

The setting applies to every project; --project sets it for this project only.

  memcode session retention               # show the current setting
  memcode session retention 90            # keep sessions for 90 days
  memcode session retention 0             # keep sessions forever
  memcode session retention 7 --project   # this project only
  memcode session retention default       # back to the default`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetBool("project")
		cfg, err := config.Load(".")
		if err != nil && (project || !errors.Is(err, config.ErrNotInitialized)) {
			return err
		}
		if len(args) == 0 {
			days, source := config.SessionRetention(cfg)
			fmt.Printf("Sessions are kept %s (%s setting).\n", retentionPhrase(days), source)
			return nil
		}
		var val *int
		if args[0] != "default" {
			n, err := strconv.Atoi(args[0])
			if err != nil || n < 0 {
				return fmt.Errorf("retention must be a number of days (0 = forever) or \"default\"")
			}
			val = &n
		}
		if project {
			cfg.SessionRetentionDays = val
			if err := cfg.Save(); err != nil {
				return err
			}
		} else if err := config.SaveUserSessionRetention(val); err != nil {
			return err
		}
		days, source := config.SessionRetention(cfg)
		fmt.Printf("Sessions are kept %s (%s setting).\n", retentionPhrase(days), source)
		return nil
	},
}

func retentionPhrase(days int) string {
	if days <= 0 {
		return "forever"
	}
	return fmt.Sprintf("for %d day(s) after their last activity", days)
}

// autoPruneSessions applies the retention setting at startup: at most once a
// day per project, never touching the sessions in keep (the one being
// resumed) or any session open in a running process. Best-effort and quiet —
// housekeeping must never stand between the user and the session.
func autoPruneSessions(ctx context.Context, st store.Store, cfg *config.Config, keep ...string) {
	if cfg == nil {
		return
	}
	days, _ := config.SessionRetention(cfg)
	if days <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _, _ = sessionprune.MaybePrune(ctx, cfg.Root, st, sessionprune.Options{RetentionDays: days, Keep: keep})
}

func init() {
	sessionDeleteCmd.Flags().Bool("all", false, "delete every session of this project")
	sessionDeleteCmd.Flags().BoolP("yes", "y", false, "do not ask for confirmation")
	sessionPruneCmd.Flags().Int("days", 0, "retention for this run only, in days")
	sessionRetentionCmd.Flags().Bool("project", false, "set it for this project only")
	sessionCmd.AddCommand(sessionDeleteCmd, sessionPruneCmd, sessionRetentionCmd)
}
