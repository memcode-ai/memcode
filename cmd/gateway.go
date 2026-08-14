package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/memcode-ai/memcode/internal/authflow"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/gateway/importer"
	gwserver "github.com/memcode-ai/memcode/internal/gateway/server"
	"github.com/memcode-ai/memcode/internal/provider"
)

// gatewayCmd runs memcode as a long-lived gateway: the same binary that runs the
// interactive agent also hosts channel adapters (Telegram/Discord/Slack/GitHub/
// WhatsApp) that turn inbound messages into agent work and post the results back.
// Self-hosted, configured once with `memcode gateway setup` — bot tokens land in
// the global .env, non-secret settings in ~/.config/memcode/gateway.yaml.
var gatewayCmd = &cobra.Command{
	Use:   "gateway",
	Short: "Run memcode as a self-hosted gateway (chat channels → agent → reply)",
	Long: `Run memcode as a long-lived gateway.

Channels are configured once with 'memcode gateway setup'. Bot tokens are stored
in the global .env; non-secret settings in ~/.config/memcode/gateway.yaml. Each
inbound message runs as a detached agent job in the current project and the
result is posted back to the channel it came from. Runs until interrupted.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		provider.LoadDotEnv() // pull bot tokens from the global .env into the environment
		settings, err := gwconfig.Load()
		if err != nil {
			return err
		}
		if len(gwconfig.EnabledChannels()) == 0 {
			return fmt.Errorf("no channels configured — run `memcode gateway setup` first")
		}
		st, cfg, err := openProject(ctx)
		if err != nil {
			return err
		}
		defer st.Close()

		cmd.Printf("memcode gateway — %s (channels: %s)\n", cfg.Root, strings.Join(gwconfig.EnabledChannels(), ", "))
		return gwserver.Run(ctx, cfg.Root, settings, cmd.OutOrStdout())
	},
}

// gatewaySetupCmd is the interactive wizard that replaces hand-setting a pile of
// environment variables. It routes each answer the way memcode (and Hermes)
// split config: bot tokens go to the global .env, non-secret knobs to
// gateway.yaml.
var gatewaySetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure gateway channels (Telegram/Discord/Slack/GitHub/WhatsApp)",
	RunE: func(cmd *cobra.Command, args []string) error {
		provider.LoadDotEnv()
		settings, err := gwconfig.Load()
		if err != nil {
			return err
		}
		in := bufio.NewReader(os.Stdin)

		for {
			if enabled := gwconfig.EnabledChannels(); len(enabled) == 0 {
				cmd.Println("No channels configured yet.")
			} else {
				cmd.Printf("Configured: %s\n", strings.Join(enabled, ", "))
			}
			choice := strings.ToLower(strings.TrimSpace(prompt(in, cmd, "Channel to add/update [telegram/discord/slack/github/whatsapp] (blank to finish): ")))

			secrets := map[string]string{}
			if settings.Channels == nil {
				settings.Channels = map[string]gwconfig.Channel{}
			}
			switch choice {
			case "":
				p, _ := gwconfig.Path()
				cmd.Printf("Done. Tokens in the global .env; settings in %s\n", p)
				return nil
			case "telegram":
				secrets[gwconfig.EnvTelegramToken] = secret(cmd, "Bot token (from @BotFather): ")
				settings.Channels["telegram"] = gwconfig.Channel{AllowFrom: allowList(in, cmd)}
			case "discord":
				secrets[gwconfig.EnvDiscordToken] = secret(cmd, "Bot token (Discord developer portal): ")
				settings.Channels["discord"] = gwconfig.Channel{AllowFrom: allowList(in, cmd)}
			case "slack":
				secrets[gwconfig.EnvSlackAppToken] = secret(cmd, "App-level token (xapp-…): ")
				secrets[gwconfig.EnvSlackBotToken] = secret(cmd, "Bot token (xoxb-…): ")
				settings.Channels["slack"] = gwconfig.Channel{AllowFrom: allowList(in, cmd)}
			case "github":
				secrets[gwconfig.EnvGitHubSecret] = secret(cmd, "Webhook secret: ")
				// GitHub deliveries are HMAC-authenticated, so no allow-list here.
				settings.Channels["github"] = gwconfig.Channel{
					ReplyTo: strings.TrimSpace(prompt(in, cmd, "Route results to (e.g. telegram:123456, blank for none): ")),
				}
			case "whatsapp":
				cmd.Println("Note: WhatsApp stays inactive until your Meta business is verified.")
				cmd.Println("Once verified, set `whatsapp.active: true` in gateway.yaml to enable it.")
				wa := gwconfig.Channel{
					PhoneNumberID: strings.TrimSpace(prompt(in, cmd, "Phone number ID: ")),
				}
				secrets[gwconfig.EnvWhatsAppToken] = secret(cmd, "Access token: ")
				secrets[gwconfig.EnvWhatsAppVerify] = secret(cmd, "Webhook verify token: ")
				secrets[gwconfig.EnvWhatsAppSecret] = secret(cmd, "App secret (verifies inbound; required to activate): ")
				wa.AllowFrom = allowList(in, cmd)
				settings.Channels["whatsapp"] = wa
			default:
				cmd.Println("Unknown channel; pick one of telegram/discord/slack/github/whatsapp.")
				continue
			}

			if len(secrets) > 0 {
				if err := authflow.SetGlobalEnv(secrets); err != nil {
					return err
				}
				// Reflect the just-written tokens so EnabledChannels sees them this loop.
				for k, v := range secrets {
					_ = os.Setenv(k, v)
				}
			}
			if err := gwconfig.Save(settings); err != nil {
				return err
			}
			cmd.Printf("Saved %s.\n", choice)
		}
	},
}

// gatewayImportCmd migrates an existing OpenClaw configuration into memcode's
// gateway config — bring your channels over with one command instead of
// reconfiguring each by hand.
var gatewayImportCmd = &cobra.Command{
	Use:   "import [openclaw.json]",
	Short: "Import channels from an existing OpenClaw config",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		provider.LoadDotEnv() // so env-referenced credentials resolve

		arg := ""
		if len(args) == 1 {
			arg = args[0]
		}
		path, searched := openClawConfigPath(arg)
		if path == "" {
			return fmt.Errorf("no OpenClaw config found (looked in: %s); pass its path explicitly", strings.Join(searched, ", "))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		res, err := importer.FromOpenClaw(data, os.Getenv)
		if err != nil {
			return err
		}

		// Merge into the existing gateway config: set each imported channel's
		// allow-list, preserving any per-channel settings already present.
		cur, err := gwconfig.Load()
		if err != nil {
			return err
		}
		if cur.Channels == nil {
			cur.Channels = map[string]gwconfig.Channel{}
		}
		var imported []string
		for name, ch := range res.Settings.Channels {
			existing := cur.Channels[name]
			existing.AllowFrom = ch.AllowFrom
			cur.Channels[name] = existing
			imported = append(imported, name)
		}
		if err := gwconfig.Save(cur); err != nil {
			return err
		}
		if len(res.Secrets) > 0 {
			if err := authflow.SetGlobalEnv(res.Secrets); err != nil {
				return err
			}
		}

		cmd.Printf("Imported from %s\n", path)
		if len(imported) > 0 {
			cmd.Printf("Channels: %s\n", strings.Join(imported, ", "))
		}
		cmd.Printf("Credentials written to the global .env: %d\n", len(res.Secrets))
		for _, note := range res.Notes {
			cmd.Printf("  note: %s\n", note)
		}
		cmd.Println("Review with `memcode gateway setup`, then run `memcode gateway`.")
		return nil
	},
}

// openClawConfigPath resolves the OpenClaw config to import: an explicit arg, then
// OpenClaw's own default locations. Returns the found path (or "") and the list
// of locations searched.
func openClawConfigPath(arg string) (string, []string) {
	if arg != "" {
		return arg, []string{arg}
	}
	var candidates []string
	if p := os.Getenv("OPENCLAW_CONFIG_PATH"); p != "" {
		candidates = append(candidates, p)
	}
	if d := os.Getenv("OPENCLAW_STATE_DIR"); d != "" {
		candidates = append(candidates, filepath.Join(d, "openclaw.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".openclaw", "openclaw.json"),
			filepath.Join(home, ".clawdbot", "openclaw.json"), // legacy
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, candidates
		}
	}
	return "", candidates
}

// allowList prompts for the principals allowed to drive the agent through this
// channel. The gateway is default-deny, so an empty answer means no one can use
// the channel yet; "*" allows anyone who can reach it.
func allowList(in *bufio.Reader, cmd *cobra.Command) []string {
	raw := prompt(in, cmd, "Allowed users — comma-separated ids/@handles, or * for anyone (blank = no one yet): ")
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// prompt writes a prompt and reads one line.
func prompt(in *bufio.Reader, cmd *cobra.Command, label string) string {
	cmd.Print(label)
	line, _ := in.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// secret reads a value without echoing it when stdin is a terminal, falling back
// to a plain read when it isn't (piped input).
func secret(cmd *cobra.Command, label string) string {
	cmd.Print(label)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		cmd.Println()
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func init() {
	gatewayCmd.AddCommand(gatewaySetupCmd)
	gatewayCmd.AddCommand(gatewayImportCmd)
	rootCmd.AddCommand(gatewayCmd)
}
