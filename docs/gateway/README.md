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
    # tier: strong                      # route this channel to a stronger model (strong|frontier)
    # agent: personal                   # bind this channel to a persona (see agents:)
    # projects: [www]                   # restrict /project on this channel to these ids
  github:
    reply_to: "telegram:123456789"      # where CI-failure results are posted
  whatsapp:
    phone_number_id: "10012345"
    active: false                        # stays inert until Meta verification
    allow_from: ["+15555550123"]
projects:                                # written by `memcode project add`
  memcode: { path: ~/github/memcode, enabled: true }
  www:     { path: ~/github/www, enabled: true }
default_project: memcode
agents:                                  # durable personas; state in ~/.memcode/agents/<id>
  personal: { type: assistant }
  coder:    { type: coding }
schedules:
  - name: standup
    cron: "0 9 * * 1-5"                  # or  every: "24h"
    task: "Summarize yesterday's commits and open PRs"
    deliver_to: "telegram:123456789"
```

Conversations are **stateful**: each `(channel, conversation, agent)` keeps its
own agent session, so follow-up messages continue with context instead of
starting fresh — and each persona keeps its own transcript, so switching
`/agent` never inherits another persona's conversation. Per-channel `tier`
routes a channel to a stronger model (a code-review channel can run strong while
a status channel stays cheap).

## Projects

The gateway is one global daemon that can work in many repos. `memcode project
add <path>` registers a directory in the project registry; a chat message can
select among registered projects with `/project <id>` but can never manufacture
an arbitrary filesystem root — the registry (canonical, symlink-resolved paths)
is the execution boundary. `default_project` is where tasks run when a
conversation hasn't chosen, and `channels.<name>.projects` narrows a channel to
a subset of the registry, so a shared group channel can't be pointed at your
other repos.

## Agents (personas)

`agents:` declares durable personas. Each has a home at `~/.memcode/agents/<id>`
holding its own `MEMCODE.md` (instructions), `memory.md`, and `skills/` — layered
onto the run as supplemental context and an extra skill root, above whatever the
project itself provides. A channel binds to a persona with `channels.<name>.agent`,
and a conversation switches with `/agent <id>`. Each persona gets its own session
transcript per conversation.

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

## Schedules

The gateway isn't only reactive. A `schedules:` entry runs a task on a cadence
(`every: "24h"` or a `cron:` expression) and posts the result to a chat
conversation. Each fire flows through the same durable inbox and reply path as a
chat message, so scheduled work is autonomous but just as reliable.

## Run

```
memcode gateway
```

from anywhere once a `default_project` is registered (`memcode project add`);
without one it falls back to the repo it's started in. One daemon per machine —
a singleton lock enforces it, which is what lets it own single-consumer bot
tokens. It runs until interrupted (Ctrl-C).

### As a background service

To keep it running across logout/reboot instead of a foreground terminal:

```
memcode gateway install
```

This writes a launchd LaunchAgent (macOS) or systemd `--user` unit (Linux) that
runs the gateway (working dir = where you ran install, the fallback when no
`default_project` is set), and prints the command to start it.
`memcode gateway uninstall` removes it.

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
- **Visible in memcode.** Gateway activity is logged to the gateway's global
  event store (`gateway_message_received` / `job_spawned` / `result_posted` /
  `dropped` / `unauthorized`) — but an inbound chat message is never turned into
  a project objective.

Gateway state is global, never inside a repo's `.memcode`: the durable inbox and
singleton lock live in `~/.config/memcode/` (`gateway.db`, SQLite WAL) alongside
`gateway.yaml`, the global `.env`, and the gateway's own event log
(`gateway-events.db`). Back up that directory and you've backed up the gateway.

## Adding a channel

The contract is deliberately thin (`internal/channels`): a chat channel
implements `Name`, `Start` (owns its connection, delivers `Inbound`), and
`Send`. Webhook-driven surfaces (GitHub, WhatsApp) instead expose an
`http.Handler` and — if they can reply — a `Send`. Vendor SDKs stay isolated to
their own adapter package (enforced by `TestVendorSDKsOnlyInTheirAdapters`), so
a new surface is one more adapter, not a new subsystem.
