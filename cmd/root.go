package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
)

var rootCmd = &cobra.Command{
	Use:   "memcode",
	Short: "An agentic coding CLI with a persistent model of your codebase",
	Long: `memcode is an agentic coding assistant for your terminal — it keeps a live
model of your codebase and puts an agent in your terminal that uses it.

Start:
  memcode               Start an interactive coding session
  memcode agent "..."   Run a single task non-interactively

Orient:
  memcode overview      What this project is and where work stands
  memcode next          The highest-value next move
  memcode recall "..."  Find where something was decided or documented

Plan & check:
  memcode plan "..."    Research and propose a plan (makes no changes)
  memcode doctor        Check memcode's health and setup

Setup is automatic — memcode initializes and indexes a project on first run and
keeps its model fresh as you work, so you never have to. Advanced and diagnostic
commands exist but are hidden from this list; run them directly (e.g. memcode map,
memcode learn) or with --help.

Learn more at https://memcode.ai`,
	// Running `memcode` with no subcommand starts the interactive TUI agent.
	RunE: func(cmd *cobra.Command, args []string) error {
		// No flag here, so the remembered mode from config is restored (not explicit).
		chrome, _ := cmd.Flags().GetBool("chrome")
		return runInteractive(cmd.Context(), permissions.ModeAsk, false, chrome, resumeRef(cmd))
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	// --chrome on the root command too, so `memcode --chrome` (interactive) works
	// the same as `memcode agent --chrome "..."` (one-shot). Chrome launches with a
	// visible window (headed) — you can watch it work.
	rootCmd.Flags().Bool("chrome", false, "enable browser tools (full browser interaction: navigate, click, type, scroll, hover, keyboard, dropdowns, history, tabs, screenshots, console logs, JS eval) backed by a real, visible Chrome instance")
	rootCmd.Flags().BoolP("continue", "c", false, "resume the most recent session with its full conversation")
	rootCmd.Flags().String("resume", "", "resume a session by id or prefix (see `memcode session recent`)")
}

// advancedCommands are real but hidden from the default --help. They still run if
// invoked directly — we just don't clutter the cockpit with the whole surface area.
// Two groups: setup MACHINERY that memcode does itself (init/index — automatic, not
// a user's job), and power-user / diagnostic / internal tools most users never need.
var advancedCommands = map[string]bool{
	// machinery — automatic, see ensureInitialized + the startup auto-refresh
	"init": true, "index": true,
	// power-user / diagnostic / internal
	"acceptance": true, "approve": true, "capabilities": true, "claims": true,
	"context": true, "eval": true, "explore": true, "jobs": true, "learn": true,
	"map": true, "objective": true, "producers": true, "session": true,
	"sources": true, "todos": true, "why": true,
}

// hideAdvanced marks the advanced commands Hidden. Called from Execute (after every
// command's init() has registered) so the cockpit shows only what users reach for:
// agent, plan, overview, predict, recall, doctor, version.
func hideAdvanced() {
	rootCmd.CompletionOptions.HiddenDefaultCmd = true // cobra adds `completion` lazily; hide it here
	for _, c := range rootCmd.Commands() {
		if advancedCommands[c.Name()] {
			c.Hidden = true
		}
	}
}

// Execute runs the root command and exits non-zero on error. It installs a signal-cancelled
// context so Ctrl-C / SIGTERM on a NON-interactive command (agent -c, learn, eval, sync)
// cancels the turn and lets deferred cleanup run (store.Close, jobs.Finish, worktree cleanup,
// a mid-append events.jsonl flush) instead of hard-killing the process. The interactive TUI
// runs the terminal in raw mode, where Ctrl-C is delivered as a key event (vaxis handles it),
// not SIGINT — so this only bites the non-interactive paths, exactly where it's needed.
func Execute() {
	hideAdvanced()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
