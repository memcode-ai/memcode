package cmd

import (
	"context"
	"os"
	"path/filepath"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	agentrt "github.com/memcode-ai/memcode/internal/agent/runtime"
	appconfig "github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/vxui"
	"github.com/spf13/cobra"
)

// personalCmd opens the Personal Agents cockpit. There is no CLI subcommand
// surface here on purpose: every operation (create, list, show, grant a
// resource, stage/approve a policy, add a trigger, run a wake, answer a
// question, check health, pause/stop/delete) is a typed pa_* tool the
// cockpit's own model calls directly (see cmd/personal_cockpit.go and
// internal/agent/tools/personal.go) — you say what you want, in plain
// language, and it does the work. `memcode personal` is the entire interface.
var personalCmd = &cobra.Command{
	Use:   "personal",
	Short: "Open the Personal Agents cockpit — an interactive agent that manages your long-lived Personal Agents",
	Long: `Open the Personal Agents cockpit — an interactive session (like ` + "`memcode admin`" + `)
that manages your long-lived Personal Agents by conversation. Just say what
you want: create one, tell it what files or capabilities it needs, review
and approve its policy, set its wake schedule, check on it, answer a
question it's stuck on, or shut it down — all through talking to it, not
through CLI commands.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPersonalCockpit(cmd.Context())
	},
}

// runPersonalCockpit opens the interactive Personal Agents session. It mirrors
// `memcode admin`: a TUI over the memcode home, with the pa_* typed tools and no
// repo/coding tools. Personal Agent state lives under ~/.memcode/agents/<id>/.
func runPersonalCockpit(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	root := filepath.Join(home, ".memcode")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if _, err := appconfig.Init(root, false); err != nil {
		return err
	}
	cfg, err := appconfig.Load(root)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, storePath(cfg.Root))
	if err != nil {
		return err
	}
	defer st.Close()

	provider.LoadDotEnv()
	maybeRunFirstRunWizard(ctx, cfg)
	var endpoints []provider.Endpoint
	if ep, ok := cfg.ResolveEndpoint(); ok {
		endpoints = append(endpoints, ep)
	}
	prov := provider.NewFromEnvLazy(endpoints...)
	model := provider.EffectiveModel(cfg.Models.Coder)
	sess := agentrt.New(st, llm.NewRunner(prov), cfg.Root, model, permissions.ModeAsk, os.Stdout)
	sess.SetPersonal(personalExecute)
	if ep, onEndpoint := prov.Endpoint(); onEndpoint {
		sess.SetPin(ep.Model, provider.CatalogWindow(ep.Model))
	} else {
		sess.SetVendor(cfg.Vendor)
		sess.SetPin(cfg.PinnedModel, cfg.PinnedWindow)
	}
	sess.SetServingDefault(cfg.ServingDefault)
	return vxui.Run(ctx, sess, cfg.Theme)
}

func init() {
	rootCmd.AddCommand(personalCmd)
}
