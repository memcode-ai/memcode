# memcode gateway

The same `memcode` binary that runs the interactive agent can run as a
long-lived, self-hosted **gateway**: it listens on the surfaces people already
use (Telegram, Discord, Slack, Email, Signal, Matrix, Mattermost, Microsoft
Teams, Google Chat, SMS, GitHub, WhatsApp), turns each inbound message into an
agent job, and posts the result back. Coding is one use of this loop,
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

| Channel     | Secret(s) in `.env`                                        | Transport         |
|-------------|------------------------------------------------------------|-------------------|
| Telegram    | `TELEGRAM_BOT_TOKEN`                                        | Bot API long-poll |
| Discord     | `DISCORD_BOT_TOKEN`                                         | gateway websocket |
| Slack       | `SLACK_APP_TOKEN`, `SLACK_BOT_TOKEN`                        | Socket Mode       |
| Email       | `EMAIL_ADDRESS`, `EMAIL_PASSWORD`, `EMAIL_IMAP_HOST`, `EMAIL_SMTP_HOST` | IMAP poll + SMTP |
| Signal      | `SIGNAL_NUMBER` (+ optional `SIGNAL_CLI_URL`)               | signal-cli daemon (SSE + JSON-RPC) |
| Matrix      | `MATRIX_HOMESERVER`, `MATRIX_ACCESS_TOKEN`                  | client-server /sync (no E2EE v1) |
| Mattermost  | `MATTERMOST_URL`, `MATTERMOST_TOKEN`                        | websocket + REST v4 |
| MS Teams    | `TEAMS_APP_ID`, `TEAMS_APP_PASSWORD`, `TEAMS_TENANT_ID`     | Bot Framework webhook |
| Google Chat | `GOOGLE_CHAT_SA_KEY` (path) + `googlechat.audience`         | signed webhook + Chat REST |
| SMS         | `TWILIO_ACCOUNT_SID`, `TWILIO_AUTH_TOKEN`, `TWILIO_FROM_NUMBER` + `sms.webhook_url` | Twilio webhook + Messages API |
| GitHub      | `GITHUB_WEBHOOK_SECRET`                                     | inbound webhook   |
| WhatsApp    | `WHATSAPP_ACCESS_TOKEN`, `WHATSAPP_VERIFY_TOKEN`, `WHATSAPP_APP_SECRET` | Meta Cloud API |

Webhook-driven surfaces (Teams, Google Chat, SMS, GitHub, WhatsApp) mount on
the shared listener (`webhook.addr`, default `:8787`) at
`/webhook/{teams,googlechat,sms,github,whatsapp}` — expose it over HTTPS.
Email works on any mailbox — the agent's own account or a personal inbox: it
polls past a durable UID cursor with PEEK fetches, so it never sets \Seen,
never touches flags or folders, and never reads mail that predates the
connection. Dedup is keyed on `<mailbox>/<UIDVALIDITY>/<UID>` (the
provider-side ack identity); Message-ID serves threading only. Unknown direct
senders get a pairing code on the chat channels, but NOT over email
(default `channels.email.pairing: false`) — a personal inbox must never
auto-reply to strangers; flip it to true for a dedicated bot address. Email's
sender identity is the RFC From address — weaker than the other channels'
platform-verified ids, so its allow-list depends on your mailbox provider
rejecting spoofed mail (SPF/DKIM/DMARC); use a mainstream provider. Signal requires a signal-cli
daemon in native HTTP mode; Matrix v1 is plain rooms only (E2EE is a known
follow-up).

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
    # agent: personal                   # bind this channel to an agent (see agents:)
    # projects: [www]                   # restrict /project on this channel to these ids
  github:
    reply_to: "telegram:123456789"      # where CI-failure results are posted
  whatsapp:
    phone_number_id: "10012345"
    active: false                        # stays inert until Meta verification
    allow_from: ["+15555550123"]
projects:                                # written by `memcode project add`
  memcode:
    path: ~/github/memcode
    enabled: true
  www:
    path: ~/github/www
    enabled: true
default_project: memcode
agents:                                  # durable agents; identity + state in ~/.memcode/agents/<id>
  jobhunt:
    objective: "Find backend roles and keep a shortlist"  # what it works toward
    autonomous: true           # ...and may work on it unprompted (separate grant)
    browser: existing_chrome   # drive the user's own signed-in Chrome
    model: claude-haiku-4-5    # omit model to let routing pick per task
  coder:
    model: claude-sonnet-5
    reasoning: medium
schedules:
  - name: standup
    cron: "0 9 * * 1-5"                  # or  every: "24h"
    task: "Summarize yesterday's commits and open PRs"
    deliver_to: "telegram:123456789"
```

Conversations are **stateful**: each `(channel, conversation, agent)` keeps its
own agent session, so follow-up messages continue with context instead of
starting fresh — and each agent keeps its own transcript, so switching
`/agent` never inherits another agent's conversation. Per-channel `tier`
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

## Agents (agents)

`agents:` declares durable agents. Each has a home at `~/.memcode/agents/<id>`
holding its own `MEMCODE.md` (instructions), `memory.md`, and `skills/` — layered
onto the run as supplemental context and an extra skill root, above whatever the
project itself provides. A channel binds to a agent with `channels.<name>.agent`,
and a conversation switches with `/agent <id>`. Each agent gets its own session
transcript per conversation.

`objective` and `autonomous` turn an ordinary agent into one that works on its
own. They are SEPARATE grants: an objective says what the agent is for,
`autonomous: true` says it may act on that without being asked, and either is
useful without the other. An unattended run is policy-gated, journals its
consequential actions, and suspends durably rather than prompting a human who
is not there. `browser: existing_chrome` points its browser work at the user's
own signed-in Chrome instead of a fresh logged-out profile; `paused: true`
stops future unattended wakes without deleting anything.

Its policy, resource grants, and run state live in the agent home rather than
`gateway.yaml`. Manage all of it by conversation in `memcode admin`. Removing
the configuration entry does not delete the home. See
`docs/autonomous-agents.md`.

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

### Pairing

You don't have to hand-collect user ids. When an unknown sender DMs the bot, it
replies once with a one-time 6-character code (1h expiry, bounded pending set,
repeats stay silent). The operator turns the code into an `allow_from` entry:

```
memcode gateway pair               # list pending requests
memcode gateway pair approve K3QP7M
memcode gateway pair deny K3QP7M
```

The running gateway hot-reloads gateway.yaml's POLICY fields (allow-lists,
projects, agents, channel knobs) and its schedules on change, so an approval or
an edited schedule takes effect within seconds — no restart. Channel connections
are wired at startup and do not hot-reload.

## Media and voice

Inbound attachments (photos, PDFs, documents) are downloaded into a
content-addressed media spool (`~/.config/memcode/media`, pruned with the
inbox) and ride the task into the engine as native image/document blocks.
Everything downstream of the adapter addresses media by spool ID, never by
path — the spool is the trust boundary.

Voice notes are transcribed gateway-side (OpenAI `gpt-4o-mini-transcribe`
falling back to `whisper-1`, or Gemini — picked by whichever key is present)
and the transcript becomes the task text; audio never reaches the engine.
Without either key a voice-only message gets an honest "not configured" reply.
Optionally, `channels.<name>.voice_replies: in_kind|always` (default `off`)
synthesizes an OGG/Opus voice reply (OpenAI `gpt-4o-mini-tts` — the full text
is always sent alongside, code blocks are never spoken, synthesis failures
degrade to text).

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

A schedule is recurring (`cron:` or `every:`), or a one-shot (`at:` an RFC3339
time) that runs once and removes itself. `disabled: true` pauses one without
deleting it.

Manage schedules from the terminal — a running gateway picks up changes within
seconds:

```
memcode gateway schedule list
memcode gateway schedule add standup --cron "0 9 * * 1-5" --to telegram:123456 "Summarize yesterday's commits and open PRs"
memcode gateway schedule add remind --at 3h --to telegram:123456 "Remind me to review the release notes"
memcode gateway schedule show standup
memcode gateway schedule edit standup --cron "0 8 * * 1-5"
memcode gateway schedule run standup          # fire now, don't wait
memcode gateway schedule disable standup      # pause; enable resumes
memcode gateway schedule remove standup
```

A schedule can run as a specific agent (`--agent`, or `agent:` in yaml) —
bringing that agent's instructions, memory, and pinned model
(`agents.<name>.model`) — and evaluate cron in a named zone (`--tz`).

`cron` and `automations` are accepted aliases for `schedule` (OpenClaw/Hermes
muscle memory), as are `create`/`rm`/`ls`/`get`/`pause`/`resume` for the verbs.
`memcode claw migrate` and `memcode hermes migrate` carry existing cron jobs
over where the source stores them readably (Hermes jobs.json, OpenClaw's legacy
cron file); jobs in OpenClaw's internal database are reported with exact
recreate instructions — never silently dropped. MCP server configs
(`mcp_servers` / `mcp.servers`) migrate into the user-scope .mcp.json, and the
source agent's SOUL.md/IDENTITY.md become a memcode agent with the same
SOUL.md file, verbatim.

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
