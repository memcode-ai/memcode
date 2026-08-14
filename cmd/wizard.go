package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/memcode-ai/memcode/internal/authflow"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/subscription/claudesub"
	"github.com/memcode-ai/memcode/internal/subscription/codex"
	"github.com/memcode-ai/memcode/internal/subscription/copilot"
	"github.com/memcode-ai/memcode/internal/subscription/grok"
)

// The first-run wizard: the front door. Before the TUI opens for a user who
// isn't set up yet, offer every zero-cost way to get a live agent — a memcode
// account, a subscription they already have (Copilot / ChatGPT / Claude), their
// own API key, or a custom endpoint. It runs once (a marker file), only when
// interactive and not already configured, and any choice (including skipping)
// is remembered so it never nags again.

// onboardedMarkerPath is the "wizard already shown" marker, alongside the global
// env file.
func onboardedMarkerPath() string {
	env := provider.GlobalEnvPath()
	if env == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(env), "onboarded")
}

func onboarded() bool {
	p := onboardedMarkerPath()
	if p == "" {
		return true // can't track it → don't nag
	}
	_, err := os.Stat(p)
	return err == nil
}

func markOnboarded() {
	p := onboardedMarkerPath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte("1\n"), 0o600)
}

// hasBackend reports whether a usable backend is already configured (a memcode
// token, an explicit endpoint, a selected subscription source, or an exported
// own key) — in which case the wizard is unnecessary.
func hasBackend(cfg *config.Config) bool {
	if strings.TrimSpace(os.Getenv(provider.EnvAPIToken)) != "" {
		return true
	}
	if strings.TrimSpace(os.Getenv(provider.EnvCredentialSource)) != "" {
		return true
	}
	if strings.TrimSpace(os.Getenv(provider.EnvEndpointURL)) != "" {
		return true
	}
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" || strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		return true
	}
	if _, ok := cfg.ResolveEndpoint(); ok {
		return true
	}
	return false
}

// interactiveTTY reports whether stdin and stdout are both terminals — the
// wizard reads a choice, so it must not run under a pipe, in CI, or headless.
func interactiveTTY() bool {
	return isCharDevice(os.Stdin) && isCharDevice(os.Stdout)
}

func isCharDevice(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// maybeRunFirstRunWizard runs the wizard when it's first-run, interactive, and
// nothing is configured yet. It persists the choice to the global env file AND
// exports it in-process so the provider constructed next picks it up.
func maybeRunFirstRunWizard(ctx context.Context, cfg *config.Config) {
	if onboarded() || !interactiveTTY() || hasBackend(cfg) {
		return
	}
	defer markOnboarded()
	runFirstRunWizard(ctx)
}

type wizardOption struct {
	label  string
	action func(ctx context.Context) error
}

func runFirstRunWizard(ctx context.Context) {
	fmt.Print("\n  Welcome to memcode.\n  Pick how you'd like to run it — you can change this anytime.\n\n")

	var opts []wizardOption
	add := func(label string, action func(context.Context) error) {
		opts = append(opts, wizardOption{label, action})
	}

	// A subscription the machine already has costs the user nothing at the
	// margin, so when one is detected it leads the menu as the recommended,
	// pre-selected default. Order of preference: Claude, ChatGPT, Copilot.
	// Hosted (memcode's metered product) stays available, just not the default.
	haveSub := false
	rec := func(first bool) string {
		if first {
			return "  [recommended]"
		}
		return ""
	}
	if claudesub.Available() {
		add("Use your Claude (Pro/Max) subscription — no extra cost"+rec(!haveSub), func(context.Context) error { return selectSource("claude") })
		haveSub = true
	}
	if codex.Available() {
		add("Use your ChatGPT (Codex) subscription — no extra cost"+rec(!haveSub), func(context.Context) error { return selectSource("codex") })
		haveSub = true
	}
	if copilot.Available() {
		add("Use your GitHub Copilot subscription — no extra cost"+rec(!haveSub), func(context.Context) error { return selectSource("copilot") })
		haveSub = true
	}
	if grok.Available() {
		add("Use your SuperGrok / X Premium+ subscription (Grok) — no extra cost"+rec(!haveSub), func(context.Context) error { return selectSource("grok") })
		haveSub = true
	} else {
		// Not detectable up front (memcode runs this login itself), so it is
		// offered but never pre-selected.
		add("Sign in with a SuperGrok / X Premium+ subscription (Grok) — no extra cost", useGrok)
	}
	add("Sign in to memcode (hosted — metered, no API keys)"+rec(!haveSub), func(context.Context) error {
		return runLogin()
	})
	add("Use your own API key (Anthropic or OpenAI)", func(context.Context) error { return promptOwnKey() })
	add("Point at a custom endpoint (Ollama, vLLM, a provider URL)", func(context.Context) error { return promptEndpoint() })
	add("Skip for now", func(context.Context) error {
		fmt.Print("\n  No problem — run `memcode login`, or set a key, whenever you're ready.\n\n")
		return nil
	})

	for i, o := range opts {
		fmt.Printf("  %d. %s\n", i+1, o.label)
	}
	fmt.Print("\n  Choice [1]: ")

	choice := readLine()
	idx := 0 // default: the first option
	if n := parseChoice(choice, len(opts)); n >= 0 {
		idx = n
	}
	if err := opts[idx].action(ctx); err != nil {
		fmt.Printf("\n  %v\n  You can finish setup later; opening memcode.\n\n", err)
	}
}

// useGrok makes the grok source live: it runs the device-code login when no
// stored login exists yet, then persists the selection.
func useGrok(ctx context.Context) error {
	if !grok.Available() {
		if err := grokLogin(ctx); err != nil {
			return err
		}
	}
	return selectSource("grok")
}

// grokLogin runs the xAI browser approval. It prints the verification URL (and
// tries to open it), so it works over SSH and in containers too — the approval
// can happen in any browser.
func grokLogin(ctx context.Context) error {
	return grok.Login(ctx, func(url, code string) {
		fmt.Print("\n  To continue, approve the login in your browser:\n")
		fmt.Printf("    Open: %s\n", url)
		if code != "" {
			fmt.Printf("    If prompted, enter code: %s\n", code)
		}
		if authflow.OpenBrowser(url) == nil {
			fmt.Print("    (opened your browser)\n")
		}
		fmt.Print("  Waiting for approval…\n")
	})
}

// selectSource persists a subscription source choice.
func selectSource(id string) error {
	if err := authflow.SetGlobalEnv(map[string]string{provider.EnvCredentialSource: id}); err != nil {
		return err
	}
	os.Setenv(provider.EnvCredentialSource, id)
	fmt.Printf("\n  ✓ Using your %s subscription. Saved to %s\n\n", id, provider.GlobalEnvPath())
	return nil
}

func promptOwnKey() error {
	fmt.Print("\n  Paste your API key (sk-ant-… for Anthropic, sk-… for OpenAI): ")
	key := strings.TrimSpace(readLine())
	if key == "" {
		return fmt.Errorf("no key entered")
	}
	env := "OPENAI_API_KEY"
	if strings.HasPrefix(key, "sk-ant-") {
		env = "ANTHROPIC_API_KEY"
	}
	if err := authflow.SetGlobalEnv(map[string]string{env: key}); err != nil {
		return err
	}
	os.Setenv(env, key)
	fmt.Printf("\n  ✓ Saved %s to %s\n\n", env, provider.GlobalEnvPath())
	return nil
}

func promptEndpoint() error {
	fmt.Print("\n  Endpoint base URL (e.g. http://localhost:11434/v1): ")
	url := strings.TrimSpace(readLine())
	if url == "" {
		return fmt.Errorf("no URL entered")
	}
	if err := authflow.SetGlobalEnv(map[string]string{provider.EnvEndpointURL: url}); err != nil {
		return err
	}
	os.Setenv(provider.EnvEndpointURL, url)
	fmt.Printf("\n  ✓ Endpoint saved to %s (pick a model with /model)\n\n", provider.GlobalEnvPath())
	return nil
}

func readLine() string {
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}

// parseChoice maps a 1-based entry to a 0-based index, -1 for empty/invalid.
func parseChoice(s string, n int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	v := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		v = v*10 + int(r-'0')
	}
	if v < 1 || v > n {
		return -1
	}
	return v - 1
}
