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

The choice is saved to your global config, so later runs pick it up with no
environment variable.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !interactiveTTY() {
			return fmt.Errorf("run in a terminal, or set one directly: memcode auth use <claude|codex|copilot>")
		}
		return runAuthPicker(cmd.Context())
	},
}

var authUseCmd = &cobra.Command{
	Use:   "use <claude|codex|copilot>",
	Short: "Use a subscription you already have, and remember it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		src, ok := canonicalSource(args[0])
		if !ok {
			return fmt.Errorf("unknown source %q — use one of: claude, codex, copilot", args[0])
		}
		if !sourceAvailable(src) {
			fmt.Printf("  Note: no %s login found yet. Saving your choice anyway — sign in to that tool and memcode will use it.\n", src)
		}
		return selectSource(src)
	},
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show how memcode is currently signed in",
	RunE: func(cmd *cobra.Command, args []string) error {
		switch {
		case strings.TrimSpace(os.Getenv(provider.EnvCredentialSource)) != "":
			src, _ := canonicalSource(os.Getenv(provider.EnvCredentialSource))
			fmt.Printf("  Using your %s subscription.\n", src)
		case strings.TrimSpace(os.Getenv(provider.EnvAPIToken)) != "":
			fmt.Printf("  Signed in to a memcode account.\n")
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
	authCmd.AddCommand(authUseCmd, authStatusCmd)
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
	}
	return false
}

// runAuthPicker shows the sign-in menu (the wizard's choices, without the
// first-run framing) and runs the selected action.
func runAuthPicker(ctx context.Context) error {
	fmt.Print("\n  How should memcode sign in?\n\n")

	var opts []wizardOption
	add := func(label string, action func(context.Context) error) {
		opts = append(opts, wizardOption{label, action})
	}
	if claudesub.Available() {
		add("Use your Claude (Pro/Max) subscription", func(context.Context) error { return selectSource("claude") })
	}
	if codex.Available() {
		add("Use your ChatGPT (Codex) subscription", func(context.Context) error { return selectSource("codex") })
	}
	if copilot.Available() {
		add("Use your GitHub Copilot subscription", func(context.Context) error { return selectSource("copilot") })
	}
	add("Sign in to memcode (hosted, metered)", func(context.Context) error { return runLogin() })
	add("Use your own API key (Anthropic or OpenAI)", func(context.Context) error { return promptOwnKey() })
	add("Point at a custom endpoint (Ollama, vLLM, a provider URL)", func(context.Context) error { return promptEndpoint() })

	for i, o := range opts {
		fmt.Printf("  %d. %s\n", i+1, o.label)
	}
	fmt.Print("\n  Choice [1]: ")

	idx := 0
	if n := parseChoice(readLine(), len(opts)); n >= 0 {
		idx = n
	}
	return opts[idx].action(ctx)
}
