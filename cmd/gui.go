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

func init() {
	guiModelsCmd.Flags().Bool("json", true, "output JSON (default; only format)")
	guiModelsCmd.Flags().Bool("pinnable", false, "only models offered in the picker")
	guiStatusCmd.Flags().Bool("json", true, "output JSON (default; only format)")
	rootCmd.AddCommand(guiModelsCmd)
	rootCmd.AddCommand(guiStatusCmd)
}
