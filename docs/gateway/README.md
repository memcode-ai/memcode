# memcode gateway

The same `memcode` binary that runs the interactive agent can run as a
long-lived, self-hosted **gateway**: it listens on the surfaces people already
use (Telegram, Discord, Slack, GitHub, WhatsApp), turns each inbound message
into an agent job, and posts the result back. Coding is one use of this loop,
not what it's built around — an inbound message is just a task.

```
event (channel/webhook) → inbound → agent job (detached subprocess) → reply
```

Each job runs as a crash-isolated subprocess (`internal/jobs`), so a hung or
panicking run can't wedge the gateway or the other channels.

## Configure

One command, not a pile of environment variables:

```
memcode gateway setup
```

It routes each answer the way memcode splits configuration:

- **Secrets** (bot tokens) → the global `.env`
  (`~/.config/memcode/.env`), never hand-set.
- **Non-secret settings** → `~/.config/memcode/gateway.yaml`.

A channel is enabled when its secret is present.

Credentials use each platform's **own conventional variable name** (no `MEMCODE_`
prefix), so you can paste the value straight from the platform's own docs — and a
config exported from another gateway (Hermes, OpenClaw) drops in unchanged.

| Channel  | Secret(s) in `.env`                        | Settings in `gateway.yaml`            | Transport         |
|----------|--------------------------------------------|---------------------------------------|-------------------|
| Telegram | `TELEGRAM_BOT_TOKEN`                        | —                                     | Bot API long-poll |
| Discord  | `DISCORD_BOT_TOKEN`                         | —                                     | gateway websocket |
| Slack    | `SLACK_APP_TOKEN`, `SLACK_BOT_TOKEN`        | —                                     | Socket Mode       |
| GitHub   | `GITHUB_WEBHOOK_SECRET`                     | `github.reply_to`                     | inbound webhook   |
| WhatsApp | `WHATSAPP_ACCESS_TOKEN`, `WHATSAPP_VERIFY_TOKEN` | `whatsapp.phone_number_id`, `…active` | Meta Cloud API |

## Run

```
memcode gateway
```

in the project the agent should operate in. It runs until interrupted (Ctrl-C).

Chat channels connect outbound (no public URL needed). GitHub and WhatsApp are
inbound webhooks served on `:8787` by default (`webhook.addr` in
`gateway.yaml`); expose that endpoint over HTTPS (a tunnel in local dev) and
point the platform's webhook at `/webhook/github` or `/webhook/whatsapp`.

### GitHub

GitHub is an event source, not a chat surface. A failed `workflow_run` becomes
an agent task; the result is routed to the chat conversation named by
`github.reply_to` (e.g. `telegram:123456`). Deliveries are authenticated by
HMAC-SHA256 over the raw body and de-duplicated on `X-GitHub-Delivery`;
memcode's own bot and `memcode/*` branches are ignored so a fix run can't
trigger itself.

### WhatsApp

WhatsApp is built but stays **inert** until your Meta business is verified —
that's an external account state the gateway can't observe. Configure it now,
then set `whatsapp.active: true` in `gateway.yaml` once verification is complete.

## Adding a channel

The contract is deliberately thin (`internal/channels`): a chat channel
implements `Name`, `Start` (owns its connection, delivers `Inbound`), and
`Send`. Webhook-driven surfaces (GitHub, WhatsApp) instead expose an
`http.Handler` and — if they can reply — a `Send`. Vendor SDKs stay isolated to
their own adapter package (enforced by `TestVendorSDKsOnlyInTheirAdapters`), so
a new surface is one more adapter, not a new subsystem.
