package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/runtime"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/structure"
)

var evalCmd = &cobra.Command{
	Use:   "eval <task>",
	Short: "A/B test the agent with the context pack vs. cold (the core proof)",
	Long: `Runs the SAME task twice in isolated git worktrees — once with memcode's
context pack ("context") and once without it ("cold", a vanilla tool-using
agent) — then compares iterations, tool calls, tokens, files touched, and
whether verification passed. This measures the central claim: does memcode's
state make the agent better?

Runs in allow-all mode (non-interactive) on throwaway worktrees at HEAD.
Requires MEMCODE_API_TOKEN (from the environment or a gitignored .env at the repo root; set MEMCODE_API_URL too). Spends tokens (two full agent runs).`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		task := joinArgs(args)

		root, _, err := config.Resolve(".")
		if err != nil {
			return err
		}
		provider.LoadDotEnv()
		prov, err := provider.NewFromEnv()
		if err != nil {
			return err
		}
		model := provider.EffectiveModel(defaultCoderModel(root))

		fmt.Printf("A/B eval: %q\n", task)
		fmt.Printf("model %s · two isolated worktrees at HEAD\n\n", model)

		variants := []struct {
			name      string
			noContext bool
		}{
			{"context", false},
			{"cold", true},
		}
		results := map[string]variantResult{}
		failed := 0
		for _, v := range variants {
			fmt.Printf("running %s variant…\n", v.name)
			res, dur, err := runVariant(ctx, root, model, prov, task, v.noContext)
			if err != nil {
				failed++
				fmt.Printf("  %s variant error: %v\n", v.name, err)
			}
			results[v.name] = variantResult{res: res, dur: dur, err: err}
		}

		printComparison(results["context"], results["cold"])
		if failed == len(variants) {
			return fmt.Errorf("all %d eval variants failed", failed) // non-zero exit for CI
		}
		return nil
	},
}

type variantResult struct {
	res runtime.Result
	dur time.Duration
	err error
}

// runVariant runs the agent against a throwaway worktree checked out at HEAD.
func runVariant(ctx context.Context, repoRoot, model string, prov provider.ModelProvider, task string, noContext bool) (runtime.Result, time.Duration, error) {
	wt, err := os.MkdirTemp("", "memcode-eval-")
	if err != nil {
		return runtime.Result{}, 0, err
	}
	defer func() {
		_ = exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", wt).Run()
		_ = os.RemoveAll(wt)
	}()

	if out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "add", "--detach", wt, "HEAD").CombinedOutput(); err != nil {
		return runtime.Result{}, 0, fmt.Errorf("worktree add: %v: %s", err, out)
	}

	if _, err := config.Init(wt, false); err != nil {
		return runtime.Result{}, 0, err
	}
	st, err := store.Open(ctx, filepath.Join(wt, config.DirName, "state.db"))
	if err != nil {
		return runtime.Result{}, 0, err
	}
	defer st.Close()
	if _, err := structure.Scan(ctx, st, wt); err != nil {
		return runtime.Result{}, 0, err
	}

	sess := runtime.New(st, llm.NewRunner(prov), wt, model, permissions.ModeAllowAll, io.Discard)
	sess.SetNoContext(noContext)

	start := time.Now()
	res, err := sess.Run(ctx, task)
	return res, time.Since(start), err
}

func printComparison(a, b variantResult) {
	fmt.Printf("\n%-16s %-12s %-12s\n", "", "context", "cold")
	row := func(label string, x, y string) {
		fmt.Printf("  %-14s %-12s %-12s\n", label, x, y)
	}
	row("verified", checkmark(a.res.Verified), checkmark(b.res.Verified))
	row("iterations", itoa(a.res.Iterations), itoa(b.res.Iterations))
	row("tool calls", itoa(a.res.ToolCalls), itoa(b.res.ToolCalls))
	row("wrong turns", itoa(a.res.WrongTurns), itoa(b.res.WrongTurns))
	row("files read", itoa(a.res.FilesRead), itoa(b.res.FilesRead))
	row("files changed", itoa(len(a.res.FilesChanged)), itoa(len(b.res.FilesChanged)))
	row("diff lines", itoa(a.res.DiffLines), itoa(b.res.DiffLines))
	row("input tokens", itoa(a.res.InputTokens), itoa(b.res.InputTokens))
	row("output tokens", itoa(a.res.OutputTokens), itoa(b.res.OutputTokens))
	row("duration", a.dur.Round(time.Second).String(), b.dur.Round(time.Second).String())

	fmt.Printf("\n%s\n", verdict(a, b))
}

// verdict weights correctness and wrong turns above raw token count: context may
// cost more tokens yet win by being more correct or taking fewer wrong turns.
func verdict(a, b variantResult) string {
	switch {
	case a.res.Verified && !b.res.Verified:
		return "→ context completed & verified where cold did NOT — context wins."
	case !a.res.Verified && b.res.Verified:
		return "→ cold verified where context did not — investigate (context may be misleading)."
	case a.res.WrongTurns < b.res.WrongTurns:
		return fmt.Sprintf("→ context took fewer wrong turns (%d vs %d) — context wins.", a.res.WrongTurns, b.res.WrongTurns)
	case a.res.WrongTurns > b.res.WrongTurns:
		return fmt.Sprintf("→ cold took fewer wrong turns (%d vs %d).", b.res.WrongTurns, a.res.WrongTurns)
	case a.res.ToolCalls != b.res.ToolCalls:
		lo, hi, who := a.res.ToolCalls, b.res.ToolCalls, "context"
		if b.res.ToolCalls < a.res.ToolCalls {
			lo, hi, who = b.res.ToolCalls, a.res.ToolCalls, "cold"
		}
		return fmt.Sprintf("→ %s used fewer tool calls (%d vs %d).", who, lo, hi)
	default:
		return "→ comparable on this task; try a larger/less obvious one."
	}
}

func checkmark(b bool) string {
	if b {
		return "✓"
	}
	return "✗"
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// defaultCoderModel reads the configured coder model, falling back to the
// provider default if the project isn't initialized.
func defaultCoderModel(root string) string {
	if cfg, err := config.Load(root); err == nil && cfg.Models.Coder != "" {
		return cfg.Models.Coder
	}
	return provider.DefaultModel(provider.TierCoder)
}

func init() {
	rootCmd.AddCommand(evalCmd)
}
