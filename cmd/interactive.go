package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/memcode-ai/memcode/internal/agent/acceptance"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/update"
	"github.com/memcode-ai/memcode/internal/vxui"
)

// runInteractive opens the project, builds a model-backed agent session, and
// launches the Bubble Tea TUI over it. Shared by `memcode` (no subcommand) and
// `memcode run` (no task). resume ("" = fresh) is a session ref — "latest"
// or an id/prefix — whose saved transcript the TUI re-enters.
func runInteractive(ctx context.Context, mode permissions.Mode, modeExplicit bool, chrome bool, resume string) error {
	st, cfg, err := openProject(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	// Restore the remembered permission mode unless the user forced one with a flag
	// this run (an explicit --auto/--allow-all is a one-off, not a new default).
	if !modeExplicit {
		if saved, ok := parseMode(cfg.Mode); ok {
			mode = saved
		}
	}

	// Reconcile the fate of prior agent work (git) so the room reducer starts
	// with fresh acceptance signal.
	_, _ = acceptance.Reconcile(ctx, st, cfg.Root)

	provider.LoadDotEnv()
	// First-run front door: if nothing is configured yet, offer the zero-cost
	// ways in (a memcode account, an existing subscription, an own key, an
	// endpoint) before the TUI opens. Runs once, interactive only; a choice is
	// exported in-process so the provider built below picks it up.
	maybeRunFirstRunWizard(ctx, cfg)
	// Mandatory-login boot: the TUI ALWAYS opens. Signed-out gets a banner
	// notice + a whitelist of local commands; /login swaps credentials into
	// this lazy provider without a restart. (Non-interactive commands keep the
	// hard NewFromEnv gate — they can't host a login flow.) Backend selection
	// (one-wire Phase C): memcode token → hosted; else the project's resolved
	// custom endpoint (env MEMCODE_ENDPOINT_URL or the config endpoint list);
	// else signed out.
	var endpoints []provider.Endpoint
	if ep, ok := cfg.ResolveEndpoint(); ok {
		endpoints = append(endpoints, ep)
	}
	prov := provider.NewFromEnvLazy(endpoints...)

	model := provider.EffectiveModel(cfg.Models.Coder)
	// NOTE: launching the CLI must NEVER start a GPU — opening the TUI is not
	// demand. The pool warms as a side effect of the first real cheap-tier turn
	// (the gateway's router starts it under a lease); there is deliberately no
	// client-triggered warmup.
	runner := llm.NewRunner(prov)
	// os.Stdout is a placeholder; tui.Run redirects output through SetOutput.
	sess := runtime.New(st, runner, cfg.Root, model, mode, os.Stdout)
	sess.SetPersonality(cfg.Personality) // remembered agent voice (tone only)
	sess.SetExtraMile(cfg.ExtraMile)     // remembered "extra mile" mode (above-and-beyond)
	if ep, onEndpoint := prov.Endpoint(); onEndpoint {
		// Endpoint mode: the hosted pin/vendor are GATEWAY label namespaces —
		// meaningless (or worse, misrouting) against an arbitrary endpoint. The
		// endpoint's resolved model IS the session model; its window comes from
		// the embedded catalog when the id is known, else the serve teaches it.
		sess.SetPin(ep.Model, provider.CatalogWindow(ep.Model))
	} else {
		// The session's model: session override -> workspace -> user -> the
		// default_model seed (persisted on first use). ResolvePin is the ONLY
		// place that chain lives.
		pin, win := config.ResolvePin(cfg, "")
		sess.SetPin(pin, win)
	}
	sess.SetServingDefault(cfg.ServingDefault)                        // cached cheap-lane model → banner/footer show it at once (refreshed by the /models fetch)
	sess.SetScoutModel(provider.EffectiveModel(cfg.Models.Explorer))  // cheap read-only scouts (Haiku)
	sess.SetPlannerModel(provider.EffectiveModel(cfg.Models.Planner)) // reasoning model for plan SYNTHESIS
	sess.SetPlanResearchModel(model)                                  // cheap model for plan-mode research
	if chrome {
		sess.SetBrowserEnabled(true)
		defer sess.CloseBrowser() // tear down Chrome when the TUI session ends
	}
	if resume != "" {
		id, err := runtime.ResolveSession(cfg.Root, resume)
		if err != nil {
			return err
		}
		sess.SetResume(id) // consumed by StartChat inside the TUI
	}
	// Boot contract (Tim): CHECK, UPDATE, RESTART — every launch. A bounded
	// network check; a newer release installs synchronously and the process
	// re-execs into it before the TUI starts. Offline falls back to any
	// previously staged binary instantly.
	update.SyncUpdate(ctx)
	// The background path remains only for MEMCODE_AUTO_UPDATE=off users
	// (a passive notice): check, download,
	// verify, and atomically stage the new binary on disk — the running
	// process is untouched (local state decides boot UX; launch never waits
	// on the network), and the NEXT launch runs the new version. Its line
	// (staged, or the manual nudge when MEMCODE_AUTO_UPDATE=off) prints
	// AFTER the TUI releases the terminal, never into it.
	noticeCh := make(chan string, 1)
	go func() { noticeCh <- update.Auto(ctx) }()

	// vaxis is the renderer (the inline-glitch-free replacement for the retired Bubble Tea fork).
	runErr := vxui.Run(ctx, sess, cfg.Theme)
	select {
	case n := <-noticeCh:
		if n != "" {
			fmt.Fprintln(os.Stderr, n)
		}
	default: // check still in flight or failed — never wait on it
	}
	return runErr
}
