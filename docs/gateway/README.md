# memcode gateway

The same `memcode` binary that runs the interactive agent can run as a
long-lived, self-hosted **gateway**: it listens on the surfaces people already
use (Telegram, Discord, Slack, GitHub, WhatsApp), turns each inbound message
into an agent job, and posts the result back. Coding is one use of this loop,
not what it's built around — an inbound message is just a task.

```
event (channel/webhook) → authorize → dedup → agent job (detached) → reply
```

Each job runs as a crash-isolated subprocess, so a hung or panicking run can't
wedge the gateway or the other channels.

## Configure

One command, not a pile of environment variables:

```
memcode gateway setup
```

It routes each answer the way memcode splits configuration:

- **Secrets** (bot tokens) → the global `.env` (`~/.config/memcode/.env`), never
  hand-set. Each uses the platform's own conventional variable name, so you can
  paste it straight from the platform's docs.
- **Non-secret settings** (allow-lists, routing) → `~/.config/memcode/gateway.yaml`.

A channel is enabled when its secret is present.

| Channel  | Secret(s) in `.env`                                        | Transport         |
|----------|------------------------------------------------------------|-------------------|
| Telegram | `TELEGRAM_BOT_TOKEN`                                        | Bot API long-poll |
| Discord  | `DISCORD_BOT_TOKEN`                                         | gateway websocket |
| Slack    | `SLACK_APP_TOKEN`, `SLACK_BOT_TOKEN`                        | Socket Mode       |
| GitHub   | `GITHUB_WEBHOOK_SECRET`                                     | inbound webhook   |
| WhatsApp | `WHATSAPP_ACCESS_TOKEN`, `WHATSAPP_VERIFY_TOKEN`, `WHATSAPP_APP_SECRET` | Meta Cloud API |

### gateway.yaml

Settings are grouped per channel (the same shape Hermes and OpenClaw use), so a
channel's allow-list and its knobs live together:

```yaml
# Anyone who can message a channel? No — default-deny. Allow-list each channel.
allow_all: false
webhook:
  addr: ":8787"          # inbound listener for GitHub/WhatsApp
channels:
  telegram:
    allow_from: ["123456789"]           # STABLE user ids (not @handles); "*" = anyone
    # respond_to_all: true              # act on every message in a group (default: mention required)
  github:
    reply_to: "telegram:123456789"      # where CI-failure results are posted
  whatsapp:
    phone_number_id: "10012345"
    active: false                        # stays inert until Meta verification
    allow_from: ["+15555550123"]
```

## Authorization and triggering

Two independent checks gate a chat message, matching what Hermes and OpenClaw do:

- **Who** — the gateway is **default-deny**: a message is dropped unless its
  sender is in that channel's `allow_from` (or `allow_all: true`). Authorization
  is on the sender's **stable id**, never the mutable @handle, so a renamed or
  lookalike handle can't gain or lose access.
- **When** — a **direct message always triggers**; in a group or channel the bot
  acts only when it's **addressed** (@mentioned or replied-to), so ordinary
  chatter doesn't spawn agent jobs. Set `respond_to_all: true` on a channel to
  act on every message. Mention detection is structural (Telegram message
  entities, Discord mentions, Slack `<@BOTID>`), not substring.

Signature-verified webhooks (GitHub) skip both — their HMAC already authenticates
the sender.

## Import from OpenClaw

Already running OpenClaw? Bring your channels over with one command:

```
memcode gateway import [path/to/openclaw.json]
```

It maps each supported channel's credentials to the matching `.env` keys and its
allow-list to `channels.<name>.allow_from`, finding the config at OpenClaw's
default locations when no path is given. Anything it can't carry automatically
(credentials behind an external secret provider, unset env references,
unsupported channels, WhatsApp's non-transferable QR session) is reported as a
note — never silently dropped.

## Run

```
memcode gateway
```

in the project the agent should operate in. It runs until interrupted (Ctrl-C).

Chat channels connect outbound (no public URL needed). GitHub and WhatsApp are
inbound webhooks served on `:8787` by default (`webhook.addr`); expose that
endpoint over HTTPS (a tunnel in local dev) and point the platform's webhook at
`/webhook/github` or `/webhook/whatsapp`.

### GitHub

GitHub is an event source, not a chat surface. A failed `workflow_run` becomes an
agent task; the result is routed to the chat conversation named by
`github.reply_to` (e.g. `telegram:123456`). Deliveries are authenticated by
HMAC-SHA256 over the raw body and de-duplicated on `X-GitHub-Delivery`;
memcode's own bot and `memcode/*` branches are ignored so a fix run can't trigger
itself.

### WhatsApp

WhatsApp is built but stays **inert** until your Meta business is verified — an
external account state the gateway can't observe. Configure it now, set the app
secret (inbound POSTs are signature-verified), then set `whatsapp.active: true`
in `gateway.yaml` once verification is complete.

## Reliability

The gateway is built around the invariants that a message-driven agent needs to
be correct, not just to demo — the failure modes both Hermes and OpenClaw hit
repeatedly:

- **Durable inbox (at-least-once).** Every accepted message is written to a durable
  SQLite inbox *before* the provider is acknowledged (Telegram advances its offset,
  Slack acks the socket, GitHub/WhatsApp return 2xx only after the write). A worker
  drains the inbox and replays anything a crash left pending, so a message is never
  lost between ack and execution. The inbox `(channel, message_id)` key is the
  dedup: a redelivery after a restart, reconnect, or provider retry is dropped,
  never re-run as a fresh paid turn. A job is marked done only after it completes,
  so at worst a crash re-runs an *interrupted* job.
- **Per-conversation ordering, bounded concurrency.** One conversation's messages
  are handled one at a time in order; a global cap keeps a flood from spawning
  unbounded agent subprocesses.
- **Durable poll offset.** Telegram's ack cursor is persisted, so a restart
  resumes where it left off instead of replaying the backlog.
- **Resilient reconnect.** Transient errors back off exponentially with jitter,
  capped, so a poll can't resonate with the server's session TTL.
- **One egress.** All outbound text (Telegram, Discord, Slack) goes through a
  single length-aware chunker; sends honor rate-limit `retry_after` instead of
  hammering.
- **Authenticated webhooks.** GitHub and WhatsApp POSTs are HMAC-verified against
  their secrets; the verification handshake is a separate path from the
  per-message signature check.
- **Visible in memcode.** Gateway activity is logged to the main event store
  (`gateway_message_received` / `job_spawned` / `result_posted` / `dropped` /
  `unauthorized`) — but an inbound chat message is never turned into a project
  objective.

State lives in the project's `.memcode/gateway.db` (SQLite, WAL) — copyable with
the rest of `.memcode`.

## Adding a channel

The contract is deliberately thin (`internal/channels`): a chat channel
implements `Name`, `Start` (owns its connection, delivers `Inbound`), and
`Send`. Webhook-driven surfaces (GitHub, WhatsApp) instead expose an
`http.Handler` and — if they can reply — a `Send`. Vendor SDKs stay isolated to
their own adapter package (enforced by `TestVendorSDKsOnlyInTheirAdapters`), so
a new surface is one more adapter, not a new subsystem.
