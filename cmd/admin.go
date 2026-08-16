package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	agentrt "github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/atomicfile"
	"github.com/memcode-ai/memcode/internal/config"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	gwstate "github.com/memcode-ai/memcode/internal/gateway/state"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/vxui"
)

// adminCmd opens the admin session: the same TUI as `memcode`, but it is the
// control room for your agents and gateway, not a coding session. Its toolset
// is the typed gw_* operations plus a file surface for agent homes; there
// are no repo/coding tools. It runs from anywhere — its home is ~/.memcode,
// not the current repo.
var adminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Configure your agents and gateway by chatting",
	Long: `Open an interactive session for administering memcode's agent side.

Ask questions about the current setup, and change it in plain language:

  "who is allowed on telegram?"
  "make me a research agent on Telegram that only Alice can use"
  "every weekday at 9am, summarize yesterday's commits to my telegram"
  "approve the pending pairing from WhatsApp"
  "install the gateway as a background service"

Changes go through typed, validated operations and take effect on the running
gateway within seconds. Secrets are never handled here: connecting a channel's
credentials stays in ` + "`memcode gateway setup`" + `.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAdmin(cmd.Context())
	},
}

func runAdmin(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// The admin session's working root is the memcode home (agent homes,
	// user-global memory) — never the current repo.
	root := filepath.Join(home, ".memcode")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if _, err := config.Init(root, false); err != nil {
		return err
	}
	cfg, err := config.Load(root)
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
	sess.SetAdmin(adminExecute)
	if ep, onEndpoint := prov.Endpoint(); onEndpoint {
		sess.SetPin(ep.Model, provider.CatalogWindow(ep.Model))
	} else {
		sess.SetVendor(cfg.Vendor)
		sess.SetPin(cfg.PinnedModel, cfg.PinnedWindow)
	}
	sess.SetServingDefault(cfg.ServingDefault)
	return vxui.Run(ctx, sess, cfg.Theme)
}

// adminServiceAction is the gw_service seam: unit-file knowledge lives here in
// cmd (shared with `memcode gateway install`), the session only orchestrates.
func adminServiceAction(ctx context.Context, action string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch action {
	case "status":
		var b strings.Builder
		if dir, err := gwconfig.Dir(); err == nil && gwstate.DaemonRunning(dir) {
			b.WriteString("A gateway is running now.\n")
		} else {
			b.WriteString("No gateway is running.\n")
		}
		path, _, start, err := gatewayUnit(runtime.GOOS, home, "", "")
		if err != nil {
			return b.String(), nil
		}
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(&b, "Background service installed: %s (start: %s)\n", path, start)
		} else {
			b.WriteString("Background service not installed.\n")
		}
		if enabled := gwconfig.EnabledChannels(); len(enabled) > 0 {
			fmt.Fprintf(&b, "Configured channels: %s\n", strings.Join(enabled, ", "))
		} else {
			b.WriteString("No channels configured yet (memcode gateway setup).\n")
		}
		return b.String(), nil
	case "install":
		bin, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locating memcode binary: %w", err)
		}
		workDir, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path, content, start, err := gatewayUnit(runtime.GOOS, home, bin, workDir)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if err := atomicfile.WriteFile(path, []byte(content), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("Installed the gateway service.\nUnit: %s\nStart it with: %s", path, start), nil
	case "uninstall":
		path, _, _, err := gatewayUnit(runtime.GOOS, home, "", "")
		if err != nil {
			return "", err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		stop := "launchctl unload " + path
		if runtime.GOOS == "linux" {
			stop = "systemctl --user disable --now memcode-gateway"
		}
		return fmt.Sprintf("Removed %s. If it is running, stop it with: %s", path, stop), nil
	}
	return "", fmt.Errorf("unknown service action %q", action)
}

func init() {
	rootCmd.AddCommand(adminCmd)
}
