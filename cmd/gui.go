package cmd

import (
	"encoding/json"
	"os"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/catalog"
	"github.com/memcode-ai/memcode/internal/buildinfo"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/wire"
)

// GUI/machine surface: JSON commands a desktop or SDK client queries so the CLI
// core stays the single source of truth — the model catalog and auth/version
// status. The `--json` flag is accepted (and default) so `memcode models --json`
// reads naturally; these commands only ever emit JSON.

var guiModelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List the supported model catalog as JSON",
	Long:  "Print the model catalog (window, pricing, vision/pdf/reasoning flags, picker metadata) as JSON. The GUI model picker reads this instead of embedding its own copy.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		all := catalog.CatalogModels()
		if pinnable, _ := cmd.Flags().GetBool("pinnable"); pinnable {
			kept := all[:0]
			for _, m := range all {
				if m.Pinnable {
					kept = append(kept, m)
				}
			}
			all = kept
		}
		return json.NewEncoder(os.Stdout).Encode(all)
	},
}

type statusJSON struct {
	LoggedIn        bool   `json:"logged_in"`
	TokenSource     string `json:"token_source"` // "environment" | a config path | "none"
	Endpoint        string `json:"endpoint"`
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	ProtocolVersion string `json:"protocol_version"`
}

var guiStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print login/endpoint/version status as JSON",
	Long:  "Print machine-readable status — login state, gateway endpoint, core version/commit, and the stream-json protocol version — for a GUI Settings/About page.",
	RunE: func(_ *cobra.Command, _ []string) error {
		provider.LoadDotEnv()
		src := provider.APITokenSource()
		endpoint := os.Getenv(provider.EnvAPIURL)
		if endpoint == "" {
			endpoint = provider.DefaultAPIURL
		}
		return json.NewEncoder(os.Stdout).Encode(statusJSON{
			LoggedIn:        src != "" && src != "none",
			TokenSource:     src,
			Endpoint:        endpoint,
			Version:         buildinfo.Version,
			Commit:          buildinfo.Commit,
			ProtocolVersion: wire.StreamJSONVersion,
		})
	},
}

type configJSON struct {
	Endpoint      string            `json:"endpoint"`
	TokenSource   string            `json:"token_source"`
	LoggedIn      bool              `json:"logged_in"`
	DefaultModels map[string]string `json:"default_models"` // tier -> model id
	BYOK          map[string]bool   `json:"byok"`           // own-key env -> present
}

var guiConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Read effective configuration as JSON",
	Long:  "Print the effective configuration a GUI Settings page reads — endpoint, login state, per-tier default models, and which direct-provider BYOK keys are present. Secret values are never emitted, only presence.",
}

var guiConfigGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Print effective configuration as JSON",
	RunE: func(_ *cobra.Command, _ []string) error {
		provider.LoadDotEnv()
		src := provider.APITokenSource()
		endpoint := os.Getenv(provider.EnvAPIURL)
		if endpoint == "" {
			endpoint = provider.DefaultAPIURL
		}
		byok := map[string]bool{}
		for _, env := range provider.OwnKeyEnvs() {
			byok[env] = os.Getenv(env) != ""
		}
		return json.NewEncoder(os.Stdout).Encode(configJSON{
			Endpoint:    endpoint,
			TokenSource: src,
			LoggedIn:    src != "" && src != "none",
			DefaultModels: map[string]string{
				string(provider.TierPlanner):     provider.DefaultModel(provider.TierPlanner),
				string(provider.TierCoder):       provider.DefaultModel(provider.TierCoder),
				string(provider.TierReviewer):    provider.DefaultModel(provider.TierReviewer),
				string(provider.TierSynthesizer): provider.DefaultModel(provider.TierSynthesizer),
				string(provider.TierClassifier):  provider.DefaultModel(provider.TierClassifier),
			},
			BYOK: byok,
		})
	},
}

func init() {
	guiModelsCmd.Flags().Bool("json", true, "output JSON (default; only format)")
	guiModelsCmd.Flags().Bool("pinnable", false, "only models offered in the picker")
	guiStatusCmd.Flags().Bool("json", true, "output JSON (default; only format)")
	guiConfigGetCmd.Flags().Bool("json", true, "output JSON (default; only format)")
	guiConfigCmd.AddCommand(guiConfigGetCmd)
	rootCmd.AddCommand(guiModelsCmd)
	rootCmd.AddCommand(guiStatusCmd)
	rootCmd.AddCommand(guiConfigCmd)
}
