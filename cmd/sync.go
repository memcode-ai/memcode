package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/overview"
	"github.com/memcode-ai/memcode/internal/provider"
	memsync "github.com/memcode-ai/memcode/internal/sync"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Write project memory to AI-editor context files (CLAUDE.md, Copilot, Cursor, Windsurf)",
	Long: `Refreshes the AI-editor context files you selected with /sync, using a freshly
synthesized project overview. Targets are remembered in .memcode/config.json.

This is the command the post-commit hook runs (` + "`memcode sync --auto`" + `), so the
files stay current as the project moves. Run it by hand any time to refresh now.

In --auto mode it is deliberately quiet and never fails a commit: if sync isn't
configured, or no API key is available, it exits 0 without writing anything.

Requires MEMCODE_API_TOKEN (from the environment or a gitignored .env at the repo root; set MEMCODE_API_URL too).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		auto, _ := cmd.Flags().GetBool("auto")

		st, cfg, err := openProject(ctx)
		if err != nil {
			if auto {
				return nil // post-commit hook: never surface project-open errors
			}
			return err
		}
		defer st.Close()

		// Nothing configured → there's nothing to do. Manual runs say so; the hook
		// stays silent (it's installed once and harmlessly fires forever).
		if !cfg.Sync.Everything && len(cfg.Sync.Targets) == 0 {
			if auto {
				return nil
			}
			return fmt.Errorf("sync isn't configured — run /sync in the interactive session to pick targets")
		}

		targets := memsync.ActiveTargets(cfg.Root, cfg.Sync)
		if len(targets) == 0 {
			if auto {
				return nil
			}
			fmt.Println("nothing to sync yet — no AI-editor files found; they'll be picked up automatically once created")
			return nil
		}

		_, runner, err := newModelRunner()
		if err != nil {
			if auto {
				return nil // no backend in a hook context — skip silently, don't break the commit
			}
			return err
		}
		model := provider.EffectiveModel(cfg.Models.Coder)

		o, err := overview.Get(ctx, st, runner, cfg.Root, model)
		if err != nil {
			if auto {
				return nil
			}
			return err
		}

		if err := memsync.Write(cfg.Root, o.Text, targets); err != nil {
			if auto {
				return nil
			}
			return err
		}

		if auto {
			return nil // quiet success in the hook
		}
		for _, t := range targets {
			fmt.Printf("synced → %s\n", t.Path)
		}
		return nil
	},
}

func init() {
	syncCmd.Flags().Bool("auto", false, "quiet best-effort mode for the post-commit hook (never fails a commit)")
	rootCmd.AddCommand(syncCmd)
}
