package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/authflow"
	"github.com/memcode-ai/memcode/internal/provider"
)

// loginCmd authenticates memcode via the browser: a local HTTP server receives
// the token from the web app's /api/cli/auth callback redirect, then writes
// MEMCODE_API_TOKEN (+ MEMCODE_API_URL) to the global env file so the existing
// LoadDotEnv → NewFromEnv path picks them up with zero changes. The flow itself
// lives in internal/authflow, shared with the TUI's /login slash command.
var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate memcode via your browser",
	Long: `Opens your browser to the memcode web app, which mints a per-org API key
and redirects back to a local server on 127.0.0.1:19090. The token is written to
your global config (~/.config/memcode/.env) so every command picks it up
automatically.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLogin()
	},
}

func init() {
	rootCmd.AddCommand(loginCmd)
}

func runLogin() error {
	fmt.Printf("\n  memcode login\n\n")
	res, err := authflow.Run(context.Background(), func(s string) {
		fmt.Printf("  %s\n", s)
	})
	if err != nil {
		return err
	}
	if err := authflow.WriteGlobalEnvToken(res.Token, res.GatewayURL); err != nil {
		return fmt.Errorf("failed to save credentials: %w", err)
	}
	// Export in-process too so anything this process does next sees the fresh
	// credentials regardless of dotenv no-override precedence.
	os.Setenv(provider.EnvAPIToken, res.Token)
	os.Setenv(provider.EnvAPIURL, res.GatewayURL)

	fmt.Printf("\n  ✓ Logged in successfully.\n")
	fmt.Printf("    Token written to %s\n", provider.GlobalEnvPath())
	fmt.Printf("    Gateway: %s\n\n", res.GatewayURL)
	return nil
}
