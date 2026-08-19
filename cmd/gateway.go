package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/memcode-ai/memcode/internal/authflow"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	gwserver "github.com/memcode-ai/memcode/internal/gateway/server"
	gwstate "github.com/memcode-ai/memcode/internal/gateway/state"
	"github.com/memcode-ai/memcode/internal/provider"
	"github.com/memcode-ai/memcode/internal/store"
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
		// Resolve the default project the gateway executes against. If one is
		// registered, use its canonical (symlink-resolved) root; otherwise fall back
		// to the current repo so `memcode gateway` in a project still works.
		var root string
		if settings.DefaultProject != "" {
			if root, err = settings.ResolveProject(settings.DefaultProject); err != nil {
				return err
			}
		} else {
			st, cfg, e := openProject(ctx)
			if e != nil {
				return e
			}
			st.Close()
			root = cfg.Root
		}

		// Gateway telemetry is gateway-operational, so it goes to a global event
		// store, never into the project's .memcode.
		gwDir, err := gwconfig.Dir()
		if err != nil {
			return err
		}
		gwEvents, err := store.Open(ctx, filepath.Join(gwDir, "gateway-events.db"))
		if err != nil {
			return err
		}
		defer gwEvents.Close()

		cmd.Printf("memcode gateway — %s (channels: %s)\n", root, strings.Join(gwconfig.EnabledChannels(), ", "))
		return gwserver.Run(ctx, root, gwEvents, settings, cmd.OutOrStdout())
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
			choice := strings.ToLower(strings.TrimSpace(prompt(in, cmd, "Channel to add/update [telegram/discord/slack/email/signal/matrix/mattermost/msteams/googlechat/sms/github/whatsapp] (blank to finish): ")))

			secrets := map[string]string{}
			if settings.Channels == nil {
				settings.Channels = map[string]gwconfig.Channel{}
			}
			// MERGE into the existing channel block — the wizard owns only the fields
			// it asks about, so hand-set knobs (agent, tier, respond_to_all, projects,
			// active, …) survive a re-run.
			ch := settings.Channels[choice]
			switch choice {
			case "":
				p, _ := gwconfig.Path()
				cmd.Printf("Done. Tokens in the global .env; settings in %s\n", p)
				return nil
			case "telegram":
				secrets[gwconfig.EnvTelegramToken] = secret(in, cmd, "Bot token (from @BotFather): ")
				ch.AllowFrom = allowList(in, cmd)
			case "discord":
				secrets[gwconfig.EnvDiscordToken] = secret(in, cmd, "Bot token (Discord developer portal): ")
				ch.AllowFrom = allowList(in, cmd)
			case "slack":
				secrets[gwconfig.EnvSlackAppToken] = secret(in, cmd, "App-level token (xapp-…): ")
				secrets[gwconfig.EnvSlackBotToken] = secret(in, cmd, "Bot token (xoxb-…): ")
				ch.AllowFrom = allowList(in, cmd)
			case "github":
				secrets[gwconfig.EnvGitHubSecret] = secret(in, cmd, "Webhook secret: ")
				// GitHub deliveries are HMAC-authenticated, so no allow-list here.
				ch.ReplyTo = strings.TrimSpace(prompt(in, cmd, "Route results to (e.g. telegram:123456, blank for none): "))
			case "whatsapp":
				cmd.Println("Note: WhatsApp stays inactive until your Meta business is verified.")
				cmd.Println("Once verified, set `whatsapp.active: true` in gateway.yaml to enable it.")
				ch.PhoneNumberID = strings.TrimSpace(prompt(in, cmd, "Phone number ID: "))
				secrets[gwconfig.EnvWhatsAppToken] = secret(in, cmd, "Access token: ")
				secrets[gwconfig.EnvWhatsAppVerify] = secret(in, cmd, "Webhook verify token: ")
				secrets[gwconfig.EnvWhatsAppSecret] = secret(in, cmd, "App secret (verifies inbound; required to activate): ")
				ch.AllowFrom = allowList(in, cmd)
			case "email":
				cmd.Println("Use a DEDICATED mailbox (an app password for Gmail/Outlook), never your personal inbox.")
				secrets[gwconfig.EnvEmailAddress] = strings.TrimSpace(prompt(in, cmd, "Email address: "))
				secrets[gwconfig.EnvEmailPassword] = secret(in, cmd, "App password: ")
				secrets[gwconfig.EnvEmailIMAPHost] = strings.TrimSpace(prompt(in, cmd, "IMAP host (e.g. imap.gmail.com): "))
				secrets[gwconfig.EnvEmailSMTPHost] = strings.TrimSpace(prompt(in, cmd, "SMTP host (e.g. smtp.gmail.com): "))
				ch.AllowFrom = allowList(in, cmd)
			case "signal":
				cmd.Println("Requires a running signal-cli daemon in HTTP mode (see the docs); use a dedicated number.")
				secrets[gwconfig.EnvSignalNumber] = strings.TrimSpace(prompt(in, cmd, "Your Signal number (+E.164): "))
				if u := strings.TrimSpace(prompt(in, cmd, "signal-cli daemon URL (blank = http://127.0.0.1:8080): ")); u != "" {
					secrets[gwconfig.EnvSignalCLIURL] = u
				}
				ch.AllowFrom = allowList(in, cmd)
			case "matrix":
				cmd.Println("Plain rooms only for now (no end-to-end-encrypted rooms).")
				secrets[gwconfig.EnvMatrixHomeserver] = strings.TrimSpace(prompt(in, cmd, "Homeserver URL (e.g. https://matrix.org): "))
				secrets[gwconfig.EnvMatrixToken] = secret(in, cmd, "Access token: ")
				ch.AllowFrom = allowList(in, cmd)
			case "mattermost":
				secrets[gwconfig.EnvMattermostURL] = strings.TrimSpace(prompt(in, cmd, "Server URL (e.g. https://mm.example.com): "))
				secrets[gwconfig.EnvMattermostToken] = secret(in, cmd, "Bot access token: ")
				ch.AllowFrom = allowList(in, cmd)
			case "msteams":
				secrets[gwconfig.EnvTeamsAppID] = strings.TrimSpace(prompt(in, cmd, "Azure app (bot) ID: "))
				secrets[gwconfig.EnvTeamsAppPassword] = secret(in, cmd, "Client secret: ")
				secrets[gwconfig.EnvTeamsTenantID] = strings.TrimSpace(prompt(in, cmd, "Tenant ID: "))
				ch.AllowFrom = allowList(in, cmd)
			case "googlechat":
				secrets[gwconfig.EnvGoogleChatSAKey] = strings.TrimSpace(prompt(in, cmd, "Path to the service-account JSON key: "))
				ch.Audience = strings.TrimSpace(prompt(in, cmd, "Project number (JWT audience): "))
				ch.AllowFrom = allowList(in, cmd)
			case "sms":
				secrets[gwconfig.EnvTwilioAccountSID] = strings.TrimSpace(prompt(in, cmd, "Twilio Account SID: "))
				secrets[gwconfig.EnvTwilioAuthToken] = secret(in, cmd, "Auth token: ")
				secrets[gwconfig.EnvTwilioFromNumber] = strings.TrimSpace(prompt(in, cmd, "Your Twilio number (+E.164): "))
				ch.WebhookURL = strings.TrimSpace(prompt(in, cmd, "Exact public webhook URL (e.g. https://gw.example.com/webhook/sms): "))
				ch.AllowFrom = allowList(in, cmd)
			default:
				cmd.Println("Unknown channel; pick one of telegram/discord/slack/email/signal/matrix/mattermost/msteams/googlechat/sms/github/whatsapp.")
				continue
			}
			settings.Channels[choice] = ch

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

// allowList prompts for the principals allowed to drive the agent through this
// channel. The gateway is default-deny, so an empty answer means no one can use
// the channel yet; "*" allows anyone who can reach it.
func allowList(in *bufio.Reader, cmd *cobra.Command) []string {
	raw := prompt(in, cmd, "Allowed users — comma-separated stable user ids (not @handles), or * for anyone (blank = no one yet): ")
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
// to a plain read when it isn't (piped input). The fallback reads through the
// SAME shared reader as prompt() — a fresh bufio.Reader here would drop lines
// the shared reader has already buffered.
func secret(in *bufio.Reader, cmd *cobra.Command, label string) string {
	cmd.Print(label)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		cmd.Println()
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

// gatewayPairCmd manages pairing requests: an unknown sender who DMs the bot
// gets a one-time code; these commands turn that code into an allow_from entry
// (or throw it away). The running gateway picks the change up within seconds —
// no restart.
var gatewayPairCmd = &cobra.Command{
	Use:   "pair",
	Short: "List pending pairing requests from unknown senders",
	RunE: func(cmd *cobra.Command, args []string) error {
		gw, err := openGatewayState(cmd.Context())
		if err != nil {
			return err
		}
		defer gw.Close()
		pending, err := gw.PendingPairings(cmd.Context(), time.Now())
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			cmd.Println("No pending pairing requests.")
			return nil
		}
		for _, p := range pending {
			cmd.Printf("%s  %s user %s  (asked %s ago)\n", p.Code, p.Channel, p.Principal, time.Since(p.CreatedAt).Round(time.Minute))
		}
		cmd.Println("\nApprove one with `memcode gateway pair approve <code>`.")
		return nil
	},
}

// approvePairing turns a taken pairing into an allow_from entry on its channel.
// Shared by the CLI (`gateway pair approve`) and the admin tool, so the
// approval semantics cannot drift. Returns already=true (and saves nothing)
// when the principal was allowed all along.
func approvePairing(p gwstate.Pairing) (already bool, err error) {
	settings, err := gwconfig.Load()
	if err != nil {
		return false, err
	}
	if settings.Channels == nil {
		settings.Channels = map[string]gwconfig.Channel{}
	}
	ch := settings.Channels[p.Channel]
	for _, id := range ch.AllowFrom {
		if id == p.Principal {
			return true, nil
		}
	}
	ch.AllowFrom = append(ch.AllowFrom, p.Principal)
	settings.Channels[p.Channel] = ch
	return false, gwconfig.Save(settings)
}

var gatewayPairApproveCmd = &cobra.Command{
	Use:   "approve <code>",
	Short: "Approve a pairing request — adds the sender to the channel's allow list",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		gw, err := openGatewayState(cmd.Context())
		if err != nil {
			return err
		}
		defer gw.Close()
		p, err := gw.TakePairing(cmd.Context(), args[0], time.Now())
		if err != nil {
			return err
		}
		already, err := approvePairing(p)
		if err != nil {
			return err
		}
		if already {
			cmd.Printf("%s user %s is already allowed.\n", p.Channel, p.Principal)
			return nil
		}
		cmd.Printf("Approved %s user %s. The gateway picks this up within a few seconds.\n", p.Channel, p.Principal)
		return nil
	},
}

var gatewayPairDenyCmd = &cobra.Command{
	Use:   "deny <code>",
	Short: "Discard a pairing request",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		gw, err := openGatewayState(cmd.Context())
		if err != nil {
			return err
		}
		defer gw.Close()
		p, err := gw.TakePairing(cmd.Context(), args[0], time.Now())
		if err != nil {
			return err
		}
		cmd.Printf("Denied %s user %s.\n", p.Channel, p.Principal)
		return nil
	},
}

// openGatewayState opens the gateway's state DB without the daemon's singleton
// lock — the pair commands only touch the pairing table, safely cross-process.
func openGatewayState(ctx context.Context) (*gwstate.Store, error) {
	dir, err := gwconfig.Dir()
	if err != nil {
		return nil, err
	}
	return gwstate.OpenShared(ctx, dir)
}

func init() {
	gatewayPairCmd.AddCommand(gatewayPairApproveCmd, gatewayPairDenyCmd)
	gatewayCmd.AddCommand(gatewaySetupCmd, gatewayPairCmd)
	rootCmd.AddCommand(gatewayCmd)
}
