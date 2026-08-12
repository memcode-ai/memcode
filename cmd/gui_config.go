package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/authflow"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/subscription/claudesub"
	"github.com/memcode-ai/memcode/internal/subscription/codex"
	"github.com/memcode-ai/memcode/internal/subscription/copilot"
)

// The write + detection half of the GUI config surface: `config set` persists a
// backend choice to the global env file (the same store the CLI wizard writes),
// and `config sources` reports what a first-run wizard needs — detected
// subscriptions and whether a backend is already configured. Together with
// `config get` these let a desktop first-run wizard do exactly what the CLI's
// does, without the app reimplementing detection or credential storage.

// settableEnv reports whether key may be written through `config set`. Only the
// backend-choice knobs are allowed: the gateway URL, a custom endpoint, a
// subscription source, and ecosystem-standard BYOK provider keys (*_API_KEY).
// The gateway TOKEN is deliberately excluded — `memcode login` owns it.
func settableEnv(key string) bool {
	switch key {
	case provider.EnvAPIURL, provider.EnvEndpointURL, provider.EnvCredentialSource:
		return true
	case provider.EnvAPIToken:
		return false
	}
	return strings.HasSuffix(key, "_API_KEY")
}

var guiConfigSetCmd = &cobra.Command{
	Use:   "set [KEY=VALUE ...]",
	Short: "Write backend configuration to the global env file",
	Long: `Persist backend configuration (endpoint, subscription source, or a BYOK
provider key) to the user-global env file (~/.config/memcode/.env), the same
store the first-run wizard writes. Pass KEY=VALUE pairs as arguments, or --stdin
to read KEY=VALUE lines from stdin (use --stdin for secrets so keys never land
in the process argument list). Only backend-choice keys are settable; the
gateway token is managed by ` + "`memcode login`" + `. Emits the keys set as JSON
(never their values).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		kv := map[string]string{}
		add := func(pair string) error {
			key, val, ok := strings.Cut(pair, "=")
			if !ok {
				return fmt.Errorf("expected KEY=VALUE, got %q", pair)
			}
			key = strings.TrimSpace(key)
			if !settableEnv(key) {
				return fmt.Errorf("%q is not a settable key (endpoint, subscription source, or *_API_KEY only; the gateway token is managed by `memcode login`)", key)
			}
			kv[key] = strings.TrimSpace(val)
			return nil
		}
		for _, a := range args {
			if err := add(a); err != nil {
				return err
			}
		}
		if useStdin, _ := cmd.Flags().GetBool("stdin"); useStdin {
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if err := add(line); err != nil {
					return err
				}
			}
			if err := sc.Err(); err != nil {
				return err
			}
		}
		if len(kv) == 0 {
			return fmt.Errorf("nothing to set (pass KEY=VALUE or --stdin)")
		}
		if err := authflow.SetGlobalEnv(kv); err != nil {
			return err
		}
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"set": keys})
	},
}

type sourceJSON struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type sourcesJSON struct {
	LoggedIn         bool         `json:"logged_in"`
	HasBackend       bool         `json:"has_backend"`
	Endpoint         string       `json:"endpoint"`          // custom endpoint (MEMCODE_ENDPOINT_URL), if any
	CredentialSource string       `json:"credential_source"` // selected subscription source, if any
	Subscriptions    []sourceJSON `json:"subscriptions"`     // detected, usable at no extra cost
}

var guiConfigSourcesCmd = &cobra.Command{
	Use:   "sources",
	Short: "Report configured backend + detected subscriptions as JSON",
	Long:  "Print what a first-run wizard needs: login state, whether any backend is configured, a custom endpoint or selected subscription source if present, and the subscriptions detected on this machine (Claude/ChatGPT/Copilot) that would cost nothing extra.",
	RunE: func(_ *cobra.Command, _ []string) error {
		provider.LoadDotEnv()
		src := provider.APITokenSource()
		loggedIn := src != "" && src != "none"
		credSource := strings.TrimSpace(os.Getenv(provider.EnvCredentialSource))
		endpoint := strings.TrimSpace(os.Getenv(provider.EnvEndpointURL))

		var subs []sourceJSON
		if claudesub.Available() {
			subs = append(subs, sourceJSON{ID: "claude", Label: "Claude (Pro/Max) subscription"})
		}
		if codex.Available() {
			subs = append(subs, sourceJSON{ID: "codex", Label: "ChatGPT (Codex) subscription"})
		}
		if copilot.Available() {
			subs = append(subs, sourceJSON{ID: "copilot", Label: "GitHub Copilot subscription"})
		}

		ownKey := false
		for _, env := range provider.OwnKeyEnvs() {
			if os.Getenv(env) != "" {
				ownKey = true
				break
			}
		}
		hasBackend := loggedIn || credSource != "" || endpoint != "" || ownKey

		return json.NewEncoder(os.Stdout).Encode(sourcesJSON{
			LoggedIn:         loggedIn,
			HasBackend:       hasBackend,
			Endpoint:         endpoint,
			CredentialSource: credSource,
			Subscriptions:    subs,
		})
	},
}

func init() {
	guiConfigSetCmd.Flags().Bool("stdin", false, "read KEY=VALUE lines from stdin (use for secrets)")
	guiConfigSetCmd.Flags().Bool("json", true, "output JSON (default; only format)")
	guiConfigSourcesCmd.Flags().Bool("json", true, "output JSON (default; only format)")
	guiConfigCmd.AddCommand(guiConfigSetCmd)
	guiConfigCmd.AddCommand(guiConfigSourcesCmd)
}
