package cmd

import (
	"github.com/spf13/cobra"

	gwserver "github.com/memcode-ai/memcode/internal/gateway/server"
	"github.com/memcode-ai/memcode/internal/provider"
)

// gatewayCmd runs memcode as a long-lived gateway: the same binary that runs the
// interactive agent also hosts channel adapters (Telegram today; Discord/Slack/
// GitHub/WhatsApp next) that turn inbound messages into agent work and post the
// results back. Self-hosted — bot tokens live in the user's global .env and
// never leave the machine. Runs until interrupted.
var gatewayCmd = &cobra.Command{
	Use:   "gateway",
	Short: "Run memcode as a self-hosted gateway (chat channels → agent → reply)",
	Long: `Run memcode as a long-lived gateway.

Configured channels (via bot tokens in the global .env, e.g.
MEMCODE_TELEGRAM_BOT_TOKEN) deliver inbound messages; each message runs as a
detached agent job in the current project and the result is posted back to the
channel it came from. The gateway runs until you interrupt it (Ctrl-C).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		provider.LoadDotEnv() // channel tokens live in the global .env

		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		cmd.Printf("memcode gateway — %s\n", cfg.Root)
		return gwserver.Run(ctx, gwserver.Config{Root: cfg.Root}, cmd.OutOrStdout())
	},
}

func init() {
	rootCmd.AddCommand(gatewayCmd)
}
