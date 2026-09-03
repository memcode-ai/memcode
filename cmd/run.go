package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/browser"
	"github.com/memcode-ai/memcode/internal/browser/broker"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/jobs"
	"github.com/memcode-ai/memcode/internal/mcp"
	"github.com/memcode-ai/memcode/internal/provider"
)

var runCmd = &cobra.Command{
	Use:   "run <task>",
	Short: "Run a coding task headlessly, oriented by memcode's context",
	Long: `Runs a coding task end-to-end: orients via "memcode context", calls
Claude, and executes the tool calls it proposes (read/search/bash/edit) under a
permission policy — recording every step as an event.

Permission modes:
  --ask        prompt before writes, installs and risky commands (default)
  --auto       run low/medium-risk commands automatically; prompt for dangerous
  --allow-all  run everything except catastrophic commands (still prompts those)

Requires MEMCODE_API_TOKEN (from the environment or a gitignored .env at the
repo root). The gateway defaults to production; MEMCODE_API_URL overrides it
for local gateway development. Never store keys in .memcode.`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		task := strings.Join(args, " ")
		mode := resolveMode(cmd)
		chrome, _ := cmd.Flags().GetBool("chrome")

		// --protocol stream-json → drive the session over the machine control protocol
		// (newline-delimited JSON on stdio) instead of the TUI. This is the surface the
		// sdk/agent wrapper speaks to; turns arrive as user_turn messages, so any task
		// args are ignored.
		if p, _ := cmd.Flags().GetString("protocol"); p == "stream-json" {
			return runStreamJSON(ctx, mode, chrome)
		}

		// No task on the command line but a piped stdin → the task IS the pipe
		// (`echo "fix the flaky test" | memcode run`). The TUI needs a real
		// terminal anyway, so a non-TTY stdin can only mean a piped prompt.
		if task == "" && !term.IsTerminal(os.Stdin.Fd()) {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 10<<20))
			if err != nil {
				return fmt.Errorf("reading task from stdin: %w", err)
			}
			task = strings.TrimSpace(string(b))
			if task == "" {
				return fmt.Errorf("stdin is not a terminal and carried no task — pipe a prompt or pass one as an argument")
			}
		}

		// No task → interactive multi-turn session in the TUI (input routing +
		// multimodal + inline approvals). It opens the project itself.
		if task == "" {
			return runInteractive(ctx, mode, modeExplicit(cmd), chrome, resumeRef(cmd))
		}

		st, cfg, prov, runner, err := openModelProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		model := provider.EffectiveModel(cfg.Models.Coder)
		// A gateway agent may pin the model that drives it (agents.<name>.model);
		// the pin rides the session's job-context envelope and replaces the config
		// default here, before session construction.
		gwSession, _ := cmd.Flags().GetString("session")
		var gwContext jobContext
		if gwSession != "" {
			gwContext = loadJobContext(gwSession)
			if gwContext.Model != "" {
				model = gwContext.Model
			}
		}
		// On a custom endpoint or a subscription source the served model is the
		// endpoint's model, not the config default — show and use that so the
		// header names what actually serves the turn.
		if ep, ok := prov.(provider.Endpointer); ok {
			if e, on := ep.Endpoint(); on && e.Model != "" {
				model = e.Model
			}
		}

		// A resumed one-shot continues a saved conversation for exactly one more
		// turn (`memcode run -c "now fix the failing test"`). Incompatible with
		// --background (the job child can't share the transcript safely).
		if resume := resumeRef(cmd); resume != "" {
			if bg, _ := cmd.Flags().GetBool("background"); bg {
				return fmt.Errorf("--background cannot be combined with --resume/--continue")
			}
		}

		// --background: detach the task as a tracked job and return immediately.
		if bg, _ := cmd.Flags().GetBool("background"); bg {
			if mode == permissions.ModeAsk {
				mode = permissions.ModeAuto // a backgrounded job can't answer prompts
				fmt.Println("note: background jobs run in --auto (can't prompt); pass --allow-all to widen")
			}
			if nc, _ := cmd.Flags().GetBool("no-context"); nc {
				fmt.Println("note: --no-context is ignored with --background (the job child builds its own context)")
			}
			reportBack, _ := cmd.Flags().GetBool("report-back")
			job, err := jobs.Spawn(cfg.Root, task, string(mode), chrome, reportBack, "")
			if err != nil {
				return err
			}
			fmt.Printf("started %s (pid %d) — follow with `memcode jobs logs %s`\n", job.ID, job.PID, job.ID)
			return nil
		}

		noContext, _ := cmd.Flags().GetBool("no-context")
		sess := runtime.New(st, runner, cfg.Root, model, mode, userOut())
		// Headless runs get a model the same way interactive ones do:
		// --model -> workspace -> user -> the default_model seed. This path used
		// to set no pin at all and rely on Automatic picking per turn; there is
		// nothing to pick with now, so the pin must be resolved here too.
		//
		// Endpoint mode is excluded for the same reason as the interactive path:
		// the hosted pin is a GATEWAY label namespace, so resolving one against
		// an arbitrary endpoint would both misroute and write a hosted label into
		// config that the endpoint never asked for.
		onEndpoint := false
		if epr, ok := prov.(provider.Endpointer); ok {
			if e, on := epr.Endpoint(); on {
				onEndpoint = true
				sess.SetPin(e.Model, provider.CatalogWindow(e.Model))
			}
		}
		if !onEndpoint {
			modelFlag, _ := cmd.Flags().GetString("model")
			pin, win := config.ResolvePin(cfg, modelFlag)
			sess.SetPin(pin, win)
			sess.SetPolicy(sessionPolicy(cfg.Root, pin))
			// The header must name the model that will actually serve. It used
			// to print a config/provider default, which under Automatic was a
			// guess and is now simply wrong.
			model = pin
		}
		sess.SetNoContext(noContext)
		browserSession, _ := cmd.Flags().GetString("browser-session")
		if chrome && browserSession != "existing_chrome" {
			sess.SetBrowserEnabled(true)
			defer sess.CloseBrowser() // tear down Chrome when the one-shot session ends
		}
		// --browser-session existing_chrome: this run is a autonomous agent's
		// delegated worker that needs the USER'S OWN already-running,
		// already-logged-in Chrome (Gmail, LinkedIn, an ATS, whatever the user
		// is signed into) — NOT a fresh ephemeral profile with no session. It
		// must acquire the gateway-owned broker's exclusive lease first;
		// failing that, it fails closed. It must NEVER silently fall back to
		// ephemeral Chrome — that would silently run the task logged out,
		// which is not what was asked for and not what the policy authorized.
		if browserSession == "existing_chrome" {
			agentID, _ := cmd.Flags().GetString("browser-agent")
			runID, _ := cmd.Flags().GetString("browser-run")
			sock, err := broker.SocketPath()
			if err != nil {
				return fmt.Errorf("existing-Chrome unavailable (%w) — refusing to fall back to ephemeral Chrome", err)
			}
			client := broker.NewClient(sock)
			lease, err := client.Acquire(agentID, runID, 10*time.Minute)
			if err != nil {
				return fmt.Errorf("existing-Chrome unavailable: %w — ask the user to run gw_browser in `memcode admin`; refusing to fall back to ephemeral Chrome", err)
			}
			defer client.Release(lease.Token)
			sess.SetExtraMCPServers(map[string]mcp.ServerConfig{
				"chrome-devtools": {Type: "stdio", Command: "npx", Args: []string{"-y", browser.ChromeDevToolsMCPPackage, "--autoConnect"}},
			})
		}
		// --allow-tools/--deny-tools: a delegated job's actual toolset restriction
		// (see jobs.SpawnSpec.ToolPolicy). Applied here — before the --job branch —
		// so it binds regardless of whether the child also carries --session.
		allowTools, _ := cmd.Flags().GetString("allow-tools")
		denyTools, _ := cmd.Flags().GetString("deny-tools")
		if allowTools != "" || denyTools != "" {
			if unknown := sess.SetToolPolicy(splitCSV(allowTools), splitCSV(denyTools)); len(unknown) > 0 {
				fmt.Printf("note: tool policy entries not recognized (see memcode.ai/docs/agents/tools): %s\n", strings.Join(unknown, ", "))
			}
		}

		// --job: this process IS a detached job's child. Serialize behind the
		// writer lock (one writer at a time) and record completion.
		if jobID, _ := cmd.Flags().GetString("job"); jobID != "" {
			release, err := jobs.AcquireWriter(ctx, cfg.Root)
			if err != nil {
				return err
			}
			defer release()
			// No human can answer approval prompts in a detached child — tools
			// whose every use would be denied are not advertised at all.
			sess.SetNoApprover(true)
			if gwSession != "" && chrome {
				// A gateway job has no desktop session: Chrome must run headless.
				sess.SetBrowserHeadless(true)
			}
			// Live readout for frontends: heartbeat activity/tokens into meta.json.
			// stopHeartbeat is synchronous and runs before EVERY Finish below, so a
			// late tick can never trample the terminal record.
			stopHeartbeat := startJobHeartbeat(ctx, sess, cfg.Root, jobID)
			// --session: a gateway conversation job. Pin the id and resume the prior
			// transcript if it exists, so follow-up messages continue the same session.
			// Uses the chat seams (which load + save the transcript) instead of Run.
			if sessionID := gwSession; sessionID != "" {
				sess.SetSessionID(sessionID)
				if gwContext.Reasoning != "" {
					sess.SetEffortOverride(gwContext.Reasoning) // agent's pinned thinking effort
				}
				if len(gwContext.Toolsets) > 0 || len(gwContext.DisabledToolsets) > 0 {
					if unknown := sess.SetToolPolicy(gwContext.Toolsets, gwContext.DisabledToolsets); len(unknown) > 0 {
						fmt.Printf("note: tool policy entries not recognized (see memcode.ai/docs/agents/tools): %s\n", strings.Join(unknown, ", "))
					}
				}
				if jc := gwContext; len(jc.Items) > 0 || len(jc.SkillRoots) > 0 || len(jc.Attachments) > 0 {
					sess.SetContext(jc.Items)                                      // gateway-supplied agent/user context for this run
					sess.SetSkillRoots(jc.SkillRoots)                              // agent's own skills join discovery
					sess.SetTaskAttachments(resolveJobAttachments(jc.Attachments)) // channel media rides this turn
				}
				if _, err := runtime.ResolveSession(cfg.Root, sessionID); err == nil {
					sess.SetResume(sessionID)
				}
				fmt.Printf("memcode job %s · model %s · mode %s · session %s\n", jobID, model, mode, sessionID)
				chat := sess.StartChat(ctx)
				sess.Submit(ctx, chat, task)
				sess.EndChat(ctx)
				code := 0
				if sess.LastError() != nil {
					code = 1
				}
				result := ""
				if rb, _ := cmd.Flags().GetBool("report-back"); rb {
					result = sess.LastText()
				}
				stopHeartbeat()
				_ = jobs.Finish(cfg.Root, jobID, code, result)
				return sess.LastError()
			}
			fmt.Printf("memcode job %s · model %s · mode %s\n", jobID, model, mode)
			_, runErr := sess.Run(ctx, task)
			code := 0
			if runErr != nil {
				code = 1
				fmt.Printf("job error: %v\n", runErr)
			}
			result := "" // a report-back job persists its final text so the caller's LLM can act on it
			if rb, _ := cmd.Flags().GetBool("report-back"); rb {
				result = sess.LastText()
			}
			stopHeartbeat()
			_ = jobs.Finish(cfg.Root, jobID, code, result)
			return runErr
		}

		// Resumed one-shot: drive the CHAT seams (StartChat consumes the saved
		// transcript, Submit runs the turn, EndChat closes the log) so the new
		// turn lands in the same conversation and re-saves it.
		if resume := resumeRef(cmd); resume != "" {
			id, err := runtime.ResolveSession(cfg.Root, resume)
			if err != nil {
				return err
			}
			sess.SetResume(id)
			fmt.Printf("memcode run · model %s · mode %s · resuming %s\n", model, mode, id)
			chat := sess.StartChat(ctx)
			sess.Submit(ctx, chat, task)
			sess.EndChat(ctx)
			return sess.LastError() // non-zero exit if the turn failed (scripting/CI)
		}

		fmt.Printf("memcode run · model %s · mode %s\n", model, mode)
		_, err = sess.Run(ctx, task)
		return err
	},
}

// splitCSV parses a comma-separated flag value into trimmed, non-empty parts.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// resumeRef reads the session-resume intent from flags: --resume <id/prefix>
// wins; --continue/-c means "the most recent saved session"; "" = fresh.
func resumeRef(cmd *cobra.Command) string {
	if r, _ := cmd.Flags().GetString("resume"); strings.TrimSpace(r) != "" {
		return strings.TrimSpace(r)
	}
	if c, _ := cmd.Flags().GetBool("continue"); c {
		return "latest"
	}
	return ""
}

func resolveMode(cmd *cobra.Command) permissions.Mode {
	if ok, _ := cmd.Flags().GetBool("allow-all"); ok {
		return permissions.ModeAllowAll
	}
	if ok, _ := cmd.Flags().GetBool("auto"); ok {
		return permissions.ModeAuto
	}
	// Ask is the default; an EXPLICIT --ask additionally counts as a forced mode
	// (see modeExplicit), so it overrides a remembered auto/allow-all for this run.
	return permissions.ModeAsk
}

// modeExplicit reports whether the user forced a mode with a flag this run (so it
// overrides — and does not overwrite — the remembered config mode). An explicit
// --ask counts: it forces prompt-mode over a remembered auto/allow-all.
func modeExplicit(cmd *cobra.Command) bool {
	a, _ := cmd.Flags().GetBool("auto")
	all, _ := cmd.Flags().GetBool("allow-all")
	ask, _ := cmd.Flags().GetBool("ask")
	return a || all || (cmd.Flags().Changed("ask") && ask)
}

// parseMode converts a stored mode string to a permissions.Mode (ok=false if it's
// empty or unrecognized).
func parseMode(s string) (permissions.Mode, bool) {
	switch s {
	case string(permissions.ModeAsk):
		return permissions.ModeAsk, true
	case string(permissions.ModeAuto):
		return permissions.ModeAuto, true
	case string(permissions.ModeAllowAll):
		return permissions.ModeAllowAll, true
	}
	return permissions.ModeAsk, false
}

func init() {
	runCmd.Flags().Bool("ask", true, "prompt before risky actions (default; pass explicitly to override a remembered mode — --ask=false is a no-op)")
	runCmd.Flags().Bool("auto", false, "run low/medium-risk actions automatically")
	runCmd.Flags().Bool("allow-all", false, "run everything except catastrophic commands")
	runCmd.Flags().Bool("no-context", false, "cold mode: skip the context pack (for A/B comparison)")
	runCmd.Flags().Bool("chrome", false, "enable browser tools (full browser interaction: navigate, click, type, scroll, hover, keyboard, dropdowns, history, tabs, screenshots, console logs, JS eval) backed by a real, visible Chrome instance")
	runCmd.Flags().Bool("background", false, "run the task as a detached background job (see `memcode jobs`)")
	runCmd.Flags().String("job", "", "internal: run as the child of a background job with this id")
	_ = runCmd.Flags().MarkHidden("job")
	runCmd.Flags().Bool("report-back", false, "internal: persist the agent's final result so the caller can report it back")
	_ = runCmd.Flags().MarkHidden("report-back")
	runCmd.Flags().String("allow-tools", "", "internal: comma-separated toolset/tool allow-list for a delegated job (empty = all)")
	_ = runCmd.Flags().MarkHidden("allow-tools")
	runCmd.Flags().String("deny-tools", "", "internal: comma-separated toolset/tool deny-list for a delegated job (deny wins)")
	_ = runCmd.Flags().MarkHidden("deny-tools")
	runCmd.Flags().String("model", "", "model for this run (a catalog label like sonnet or opus); overrides the remembered pin without changing it")
	runCmd.Flags().String("browser-session", "", "internal: \"existing_chrome\" attaches this run to the user's own already-running Chrome via the gateway browser broker (fails closed, never falls back to ephemeral)")
	_ = runCmd.Flags().MarkHidden("browser-session")
	runCmd.Flags().String("browser-agent", "", "internal: agent id for the existing-Chrome broker lease")
	_ = runCmd.Flags().MarkHidden("browser-agent")
	runCmd.Flags().String("browser-run", "", "internal: run id for the existing-Chrome broker lease")
	_ = runCmd.Flags().MarkHidden("browser-run")
	runCmd.Flags().String("protocol", "", "machine control protocol: stream-json (newline-delimited JSON on stdio, for SDK wrappers)")
	runCmd.Flags().BoolP("continue", "c", false, "resume the most recent session with its full conversation")
	runCmd.Flags().String("resume", "", "resume a session by id or prefix (see `memcode session recent`)")
	runCmd.Flags().String("session", "", "run a --job in this session id, resuming it if it exists (gateway conversation continuity)")
	rootCmd.AddCommand(runCmd)
}
