package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/subscription/claudesub"
	"github.com/memcode-ai/memcode/internal/subscription/codex"
	"github.com/memcode-ai/memcode/internal/subscription/copilot"
	"github.com/memcode-ai/memcode/internal/subscription/grok"
)

// authCmd is the front door for choosing how memcode signs in, so a user never
// has to discover MEMCODE_CREDENTIAL_SOURCE. `memcode auth` opens the same
// picker as the first-run wizard; `memcode auth use <source>` sets a
// subscription non-interactively; `memcode auth status` shows the current one.
// Every choice is persisted to the global env file, so it sticks with no env
// var on later runs.
var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Choose how memcode signs in (subscription, key, or account)",
	Long: `Choose how memcode signs in and remember it.

Run without arguments for an interactive picker, or set a subscription directly:

  memcode auth use claude     use a Claude Pro/Max subscription
  memcode auth use codex      use a ChatGPT (Codex) login
  memcode auth use copilot    use a GitHub Copilot subscription
  memcode auth use grok       use a SuperGrok / X Premium+ subscription

The choice is saved to your global config, so later runs pick it up with no
environment variable.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !interactiveTTY() {
			return fmt.Errorf("run in a terminal, or set one directly: memcode auth use <claude|codex|copilot|grok>")
		}
		return runAuthPicker(cmd.Context())
	},
}

var authDetachCmd = &cobra.Command{
	Use:   "detach <claude|codex|copilot|grok>",
	Short: "Detach a subscription — its models serve on memcode again",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		src, ok := canonicalSource(args[0])
		if !ok {
			return fmt.Errorf("unknown source %q — use one of: claude, codex, copilot, grok", args[0])
		}
		return detachSource(src)
	},
}

var authUseCmd = &cobra.Command{
	Use:   "use <claude|codex|copilot|grok>",
	Short: "Attach a subscription you already have (alias of `memcode auth <source>`)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		src, ok := canonicalSource(args[0])
		if !ok {
			return fmt.Errorf("unknown source %q — use one of: claude, codex, copilot, grok", args[0])
		}
		// Grok has no other tool's login to reuse: memcode signs in itself, so
		// selecting it without a stored login starts the browser approval.
		if src == "grok" && !grok.Available() {
			if err := grokLogin(cmd.Context()); err != nil {
				return err
			}
		}
		if src != "grok" && !sourceAvailable(src) {
			fmt.Printf("  Note: no %s login found yet. Saving your choice anyway — sign in to that tool and memcode will use it.\n", src)
		}
		return selectSource(src)
	},
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show how memcode is currently signed in",
	RunE: func(cmd *cobra.Command, args []string) error {
		attached := provider.AttachedSources()
		loggedIn := strings.TrimSpace(os.Getenv(provider.EnvAPIToken)) != ""
		if loggedIn {
			fmt.Printf("  Signed in to a memcode account (serves every model family).\n")
		}
		for _, src := range attached {
			state := "active"
			if !sourceAvailable(src) {
				state = "attached, but NOT signed in — run `memcode auth " + src + "`"
			}
			fmt.Printf("  Lane: %s subscription → %s models (%s)\n", src, provider.SourceVendor(src), state)
		}
		if loggedIn || len(attached) > 0 {
			return nil
		}
		switch {
		case strings.TrimSpace(os.Getenv(provider.EnvEndpointURL)) != "":
			fmt.Printf("  Using a custom endpoint: %s\n", strings.TrimSpace(os.Getenv(provider.EnvEndpointURL)))
		case strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "":
			fmt.Printf("  Using your exported ANTHROPIC_API_KEY.\n")
		case strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "":
			fmt.Printf("  Using your exported OPENAI_API_KEY.\n")
		default:
			fmt.Printf("  Not connected yet. Run `memcode auth` to set it up.\n")
		}
		return nil
	},
}

func init() {
	authCmd.AddCommand(authUseCmd, authDetachCmd, authStatusCmd)
	rootCmd.AddCommand(authCmd)
}

// canonicalSource maps an input (including the legacy aliases) to the canonical
// source id, ok=false when it isn't a known subscription source.
func canonicalSource(in string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "claude", "claude-sub", "anthropic-sub":
		return "claude", true
	case "codex", "chatgpt":
		return "codex", true
	case "copilot", "github-copilot":
		return "copilot", true
	case "grok", "grok-sub", "xai", "xai-sub", "supergrok":
		return "grok", true
	}
	return "", false
}

// sourceAvailable reports whether the login for a source is present on the
// machine right now.
func sourceAvailable(src string) bool {
	switch src {
	case "claude":
		return claudesub.Available()
	case "codex":
		return codex.Available()
	case "copilot":
		return copilot.Available()
	case "grok":
		return grok.Available()
	}
	return false
}

// runAuthPicker shows the sign-in menu (the wizard's choices, without the
// first-run framing) and runs the selected action.
func runAuthPicker(ctx context.Context) error {
	fmt.Print("\n  How should memcode sign in?\n\n")
	return pickOption(signInOptions(false)).action(ctx)
}
