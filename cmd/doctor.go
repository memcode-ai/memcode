package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/doctor"
	"github.com/memcode-ai/memcode/internal/provider"
)

// gitBranch returns the current branch name by shelling out to git.
// Falls back to "(unknown)" when git is unavailable or HEAD is detached.
func gitBranch(root string) string {
	if _, err := exec.LookPath("git"); err != nil {
		return "(unknown)"
	}
	out, err := exec.Command("git", "-C", root, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return "(detached HEAD)"
	}
	if b := strings.TrimSpace(string(out)); b != "" {
		return b
	}
	return "(unknown)"
}

// doctor reports the engine status
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Show the active project root, state path and engine status",
	Long: `Reports exactly which root memcode resolved and where its state lives,
so you can confirm every command operates on the same .memcode regardless of the
directory you run from.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		root, src, err := config.Resolve(".")
		if err != nil {
			return err
		}
		fmt.Printf("repo root:  %s  (%s)\n", root, src)
		fmt.Printf("branch:     %s\n\n", gitBranch(root))

		// A health check must report the reality the app runs in — so go through the
		// SAME project-open path every other command uses (init if needed, ensure the
		// .memcode self-ignore, freshen the index), not a parallel store.Open that can
		// diverge. The self-ignore check then verifies the real invariant: after the
		// app's EnsureGitignore runs, does git actually ignore .memcode?
		st, cfg, err := openProject(ctx)
		if err != nil {
			// Can't open the project (e.g. not in a git repo) — report what we can,
			// with no store, plus the blocker.
			fmt.Println(doctor.Render(doctor.Check(ctx, nil, root, nil)))
			fmt.Fprintf(os.Stderr, "\nproject not open: %v\n", err)
			return nil
		}
		defer st.Close()

		// Construct the active backend the same way the app does — dotenv, then
		// token-or-endpoint with the CONFIG-resolved endpoint passed through, so
		// a config-listed endpoint (no env vars) reports as the real backend
		// instead of "not connected". No backend configured → checks degrade
		// gracefully.
		var prov provider.ModelProvider
		provider.LoadDotEnv(cfg.Root)
		var endpoints []provider.Endpoint
		if ep, ok := cfg.ResolveEndpoint(); ok {
			endpoints = append(endpoints, ep)
		}
		if p, err := provider.NewFromEnv(endpoints...); err == nil {
			prov = p
		}

		fmt.Println(doctor.Render(doctor.Check(ctx, st, cfg.Root, prov)))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
