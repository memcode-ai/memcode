package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/jobs"
	"github.com/memcode-ai/memcode/internal/llm"
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

		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		// Load .env (gitignored) into the environment, then build the active backend.
		provider.LoadDotEnv()
		prov, err := provider.NewFromEnv()
		if err != nil {
			return err
		}

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
		runner := llm.NewRunner(prov)

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
			job, err := jobs.Spawn(cfg.Root, task, string(mode), "", chrome, false, "")
			if err != nil {
				return err
			}
			fmt.Printf("started %s (pid %d) — follow with `memcode jobs logs %s`\n", job.ID, job.PID, job.ID)
			return nil
		}

		noContext, _ := cmd.Flags().GetBool("no-context")
		sess := runtime.New(st, runner, cfg.Root, model, mode, userOut())
		sess.SetScoutModel(provider.EffectiveModel(cfg.Models.Explorer)) // cheap read-only scouts
		sess.SetNoContext(noContext)
		if chrome {
			sess.SetBrowserEnabled(true)
			defer sess.CloseBrowser() // tear down Chrome when the one-shot session ends
		}

		// --job: this process IS a detached job's child. Serialize behind the
		// writer lock (one writer at a time) and record completion.
		if jobID, _ := cmd.Flags().GetString("job"); jobID != "" {
			release, err := jobs.AcquireWriter(ctx, cfg.Root)
			if err != nil {
				return err
			}
			defer release()
			switch tier, _ := cmd.Flags().GetString("tier"); tier {
			case "frontier":
				sess.SetForceFrontier(true) // long-running background agent → top strong tier
			case "strong":
				sess.SetForceEscalate(true) // strong-tier background agent → strong vendor's balanced tier
			}
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
	return permissions.ModeAsk
}

// modeExplicit reports whether the user forced a mode with a flag this run (so it
// overrides — and does not overwrite — the remembered config mode).
func modeExplicit(cmd *cobra.Command) bool {
	a, _ := cmd.Flags().GetBool("auto")
	all, _ := cmd.Flags().GetBool("allow-all")
	return a || all
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
	runCmd.Flags().Bool("ask", true, "prompt before risky actions (default)")
	runCmd.Flags().Bool("auto", false, "run low/medium-risk actions automatically")
	runCmd.Flags().Bool("allow-all", false, "run everything except catastrophic commands")
	runCmd.Flags().Bool("no-context", false, "cold mode: skip the context pack (for A/B comparison)")
	runCmd.Flags().Bool("chrome", false, "enable browser tools (full browser interaction: navigate, click, type, scroll, hover, keyboard, dropdowns, history, tabs, screenshots, console logs, JS eval) backed by a real, visible Chrome instance")
	runCmd.Flags().Bool("background", false, "run the task as a detached background job (see `memcode jobs`)")
	runCmd.Flags().String("job", "", "internal: run as the child of a background job with this id")
	_ = runCmd.Flags().MarkHidden("job")
	runCmd.Flags().String("tier", "", "internal: model tier for a background agent child (\"strong\" → Anthropic)")
	_ = runCmd.Flags().MarkHidden("tier")
	runCmd.Flags().Bool("report-back", false, "internal: persist the agent's final result so the caller can report it back")
	_ = runCmd.Flags().MarkHidden("report-back")
	runCmd.Flags().String("protocol", "", "machine control protocol: stream-json (newline-delimited JSON on stdio, for SDK wrappers)")
	runCmd.Flags().BoolP("continue", "c", false, "resume the most recent session with its full conversation")
	runCmd.Flags().String("resume", "", "resume a session by id or prefix (see `memcode session recent`)")
	runCmd.Flags().String("session", "", "run a --job in this session id, resuming it if it exists (gateway conversation continuity)")
	rootCmd.AddCommand(runCmd)
}
