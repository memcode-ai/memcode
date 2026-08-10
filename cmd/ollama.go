package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/authflow"
	"github.com/memcode-ai/memcode/internal/provider"
)

// ollamaCmd is the friendly front door for a local Ollama server, so a user
// doesn't have to know MEMCODE_ENDPOINT_URL / MEMCODE_ENDPOINT_MODEL. It points
// memcode at Ollama's OpenAI-compatible endpoint (and optionally a model) and
// saves it, so later runs need no environment variable.
var ollamaCmd = &cobra.Command{
	Use:   "ollama [model]",
	Short: "Use a local Ollama server, optionally naming a model",
	Long: `Point memcode at a local Ollama server and remember it.

  memcode ollama                    use Ollama with the model you pick via /model
  memcode ollama qwen2.5-coder      use Ollama and default to this model

By default it uses http://localhost:11434; pass --url for a different host. The
choice is saved to your global config, so later runs pick it up with no
environment variable.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		host, _ := cmd.Flags().GetString("url")
		host = strings.TrimRight(strings.TrimSpace(host), "/")
		endpoint := host + "/v1"

		env := map[string]string{provider.EnvEndpointURL: endpoint}
		model := ""
		if len(args) == 1 {
			model = strings.TrimSpace(args[0])
			env[provider.EnvEndpointModel] = model
		}
		if err := authflow.SetGlobalEnv(env); err != nil {
			return err
		}
		os.Setenv(provider.EnvEndpointURL, endpoint)
		if model != "" {
			os.Setenv(provider.EnvEndpointModel, model)
		}

		fmt.Printf("\n  ✓ Using Ollama at %s", endpoint)
		if model != "" {
			fmt.Printf(", model %s", model)
		}
		fmt.Printf(".\n    Saved to %s\n", provider.GlobalEnvPath())
		if model == "" {
			fmt.Printf("    Pick a model in a session with /model.\n")
		}
		fmt.Printf("\n  Run `memcode` to start.\n\n")
		return nil
	},
}

func init() {
	ollamaCmd.Flags().String("url", "http://localhost:11434", "Ollama host base URL")
	rootCmd.AddCommand(ollamaCmd)
}
