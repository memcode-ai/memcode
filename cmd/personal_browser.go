package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/memcode-ai/memcode/internal/browser"
	"github.com/memcode-ai/memcode/internal/browser/broker"
	"github.com/memcode-ai/memcode/internal/mcp"
	"github.com/spf13/cobra"
)

// personalBrowserCmd groups existing-Chrome setup/diagnostics under
// `memcode personal browser`. Personal Agents default their "browser"
// toolset to the user's OWN already-running Chrome (see
// docs/design/personal-agents.md "Browser broker trust boundary"), not a
// fresh ephemeral profile — this is where that gets configured and verified.
var personalBrowserCmd = &cobra.Command{Use: "browser", Short: "Set up and check existing-Chrome access for delegated Personal Agent workers"}

var personalBrowserSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Check prerequisites and connect to your already-running Chrome",
	Long: `Personal Agents delegate browser work to your OWN already-running, already-
logged-in Chrome — not a fresh profile — so a delegated worker can actually
use accounts you're signed into (Gmail, LinkedIn, an ATS, ...). This requires:

  1. Chrome 144+.
  2. Remote Debugging enabled: open chrome://inspect/#remote-debugging in
     Chrome and toggle Remote Debugging on.
  3. The memcode gateway running (` + "`memcode gateway run`" + `) — it owns the
     broker that arbitrates which delegated worker may drive Chrome at a
     time, so at most one worker touches it at once.

This command checks each prerequisite and attempts a real connection. It
does NOT click Chrome's own "Allow" dialog for you — the first connection
attempt after this shows that dialog in Chrome itself, and only you can
approve it. If anything here fails, existing-Chrome delegation fails closed
rather than silently falling back to a fresh, logged-out browser.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		w := cmd.OutOrStdout()
		ok := true
		check := func(name string, good bool, detail string) {
			mark := "ok"
			if !good {
				mark = "FAIL"
				ok = false
			}
			fmt.Fprintf(w, "  [%s] %s: %s\n", mark, name, detail)
		}

		npx, err := exec.LookPath("npx")
		check("npx available", err == nil, func() string {
			if err != nil {
				return "not found on PATH — Node.js is required"
			}
			return npx
		}())

		sock, err := broker.SocketPath()
		if err != nil {
			check("broker socket path", false, err.Error())
		} else {
			reachable := broker.NewClient(sock).Reachable()
			check("gateway browser broker", reachable, func() string {
				if reachable {
					return sock
				}
				return "not reachable — start `memcode gateway run` first"
			}())
		}

		if !ok {
			fmt.Fprintln(w, "\nFix the above, then re-run `memcode personal browser setup`.")
			return fmt.Errorf("prerequisites not met")
		}

		fmt.Fprintln(w, "\nAttempting a connection to your running Chrome (10s timeout)...")
		fmt.Fprintln(w, "If Chrome shows an \"Allow\" dialog, click Allow — that's Chrome's own consent")
		fmt.Fprintln(w, "step, not something this command can do for you.")
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		mgr := mcp.Connect(ctx, map[string]mcp.ServerConfig{
			"chrome-devtools": {Type: "stdio", Command: "npx", Args: []string{"-y", browser.ChromeDevToolsMCPPackage, "--autoConnect"}},
		}, mcp.Options{Version: mcpSetupClientVersion})
		defer mgr.Close()
		tools := mgr.Tools()
		errs := mgr.Errors()
		if len(tools) == 0 {
			fmt.Fprintln(w, "\n  [FAIL] could not connect to Chrome")
			for _, e := range errs {
				fmt.Fprintf(w, "    - %v\n", e)
			}
			fmt.Fprintln(w, "\nCheck: Chrome 144+, chrome://inspect/#remote-debugging toggled on, Chrome")
			fmt.Fprintln(w, "actually running (autoConnect attaches to a running instance, it doesn't")
			fmt.Fprintln(w, "launch one), and that you clicked Allow if a dialog appeared.")
			return fmt.Errorf("existing-Chrome connection failed")
		}
		fmt.Fprintf(w, "\n  [ok] connected — %d browser tool(s) available\n", len(tools))
		fmt.Fprintln(w, "\nExisting-Chrome delegation is ready. A Personal Agent's delegate calls with")
		fmt.Fprintln(w, "toolsets:[\"browser\"] will now use this session by default.")
		return nil
	},
}

const mcpSetupClientVersion = "0.1.0"

func init() {
	personalBrowserCmd.AddCommand(personalBrowserSetupCmd)
	personalCmd.AddCommand(personalBrowserCmd)
}
