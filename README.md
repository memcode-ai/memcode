<p align="center">
  <img src="assets/banner.png" alt="memcode" width="100%">
</p>

<p align="center"><b>The coding agent that remembers your repo.</b></p>

<p align="center">
  <a href="https://memcode.ai"><img src="https://img.shields.io/badge/Website-memcode.ai-8A7CFF?style=for-the-badge" alt="memcode.ai"></a>
  <a href="https://github.com/memcode-ai/memcode/releases"><img src="https://img.shields.io/github/v/release/memcode-ai/memcode?style=for-the-badge&color=blue" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-green?style=for-the-badge" alt="License: MIT"></a>
</p>

Most coding agents start every session from zero. memcode keeps a persistent model of your repo in `.memcode`: the subsystems, what you worked on last week, which approaches failed and why, and the preferences you have corrected it on. The longer you use it, the less you have to explain.

One Go binary, two ways to run it. **Code** is the interactive agent in your terminal. **Agents** is the same binary as a self-hosted gateway, answering on the chat surfaces you already use. Both run against whatever models you have: your own API keys, a local endpoint like Ollama, or a hosted memcode account.

<p align="center">
  <img src="assets/screenshot-models.png" alt="memcode terminal UI: the matrix splash and the model picker, from Automatic to any specific model" width="100%">
</p>

<p align="center">
  <img src="assets/screenshot-agent.png" alt="memcode editing code while tracking a live task plan in the terminal" width="100%">
</p>

## Code

Run `memcode` in a repo and you get a full terminal coding agent.

**Remembers your repo.** Version-control-friendly state in `.memcode` that survives sessions and machines. Searchable session history. Repeated failures distill into lessons that resurface when they matter. Honors `MEMCODE.md`, `AGENTS.md`, and `CLAUDE.md` instructions, and compresses oversized ones once instead of re-reading them forever.

**Model policy in the client.** Automatic mode routes each call by what the turn needs: cheap models for routine work, strong models for planning, review, high-risk changes, and recovery after its own mistakes. Pin any model with `/model`. Failures walk catalog-defined fallback chains.

**Reads the room.** Tracks the working mood of the session. When you are correcting it, it stops cutting corners, spends more on the model, and asks before acting. When things are calm it stays out of the way.

**Plans before it builds.** `/plan` researches with parallel scouts, drafts, gets a cross-model review, and turns the approved plan into a binding contract for execution.

**Delegates and parallelizes.** Spawn read-only explorers or full sub-agents, run detached background jobs, and manage them with `/jobs`, `/tail`, `/kill`.

**A real terminal UI.** Multiline editing, slash commands with autocomplete, streaming tool output, interrupt and redirect mid-turn, themes, and a live context meter.

**Table stakes, done properly.** MCP client, Agent Skills, hooks, resident LSP for diagnostics and navigation, a sandboxed shell with a real command classifier, vision and PDF input, prompt caching, and context compaction that respects the model's actual window.

**Self-updating.** Stages updates in the background and applies them on the next launch. `MEMCODE_AUTO_UPDATE=off` keeps it manual.

## Agents

The same binary runs as a long-lived, self-hosted gateway: inbound messages become agent jobs in your repos, and the result comes back where you asked. Coding is one use of the loop, not what it is built around.

**Twelve channels.** Telegram, Discord, Slack, GitHub, WhatsApp, Email, Signal, Matrix, Mattermost, Microsoft Teams, Google Chat, and SMS. A channel is on when its secret is set; `memcode gateway setup` walks you through it.

**Voice.** Voice notes are transcribed and handled like any other message. Replies can come back as voice notes too, per channel and off by default.

**Safe by construction.** Durable idempotent dispatch, per-channel allow-lists, and a pairing flow so an unknown sender gets approved from your side before the agent answers. Each job runs as a crash-isolated subprocess.

**Easy to move in.** One-command import from OpenClaw, and `memcode gateway install` registers it as a system service.

## Install

```bash
curl -fsSL https://memcode.ai/install.sh | sh
```

Or with Go:

```bash
go install github.com/memcode-ai/memcode@latest
```

Or build from source:

```bash
git clone https://github.com/memcode-ai/memcode
cd memcode && go build -o memcode .
```

Then run `memcode` in a repo.

## Use any model

```bash
MEMCODE_ENDPOINT_URL=http://localhost:11434/v1 memcode      # Ollama, local
MEMCODE_ENDPOINT_URL=https://api.openai.com/v1 memcode      # your OpenAI key (OPENAI_API_KEY)
MEMCODE_ENDPOINT_URL=https://api.anthropic.com memcode      # your Anthropic key (ANTHROPIC_API_KEY)
```

`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `XAI_API_KEY`, `FIREWORKS_API_KEY`, and friends are picked up automatically for their hosts. Named endpoints live in config, and `/model` switches models mid-session.

With a memcode account you get one balance across every vendor, a key vault for BYOK, and hosted web search:

```bash
memcode login
```

## Documentation

The user manual lives at [memcode.ai/docs](https://memcode.ai/docs):

- [Code](https://memcode.ai/docs): commands, model routing, memory, plan mode, MCP, skills, custom instructions, configuration.
- [Agents](https://memcode.ai/docs/agents): per-channel setup guides for every gateway channel, pairing, and voice.

Internals and reference docs live in this repo:

- [ROUTING.md](ROUTING.md): how Automatic mode picks models.
- [HOOKS.md](HOOKS.md): the hook surface.
- [COMPACTION.md](COMPACTION.md): context compaction.
- [docs/gateway/README.md](docs/gateway/README.md): gateway operations and channel secrets.
- [protocol/PROTOCOL.md](protocol/PROTOCOL.md): the wire protocol every backend speaks.

## Architecture

The CLI is the agent: all model selection, escalation, and recovery run client-side; every backend is a plain serving surface speaking one OpenAI-compatible wire. Point memcode at your own API keys, a local endpoint like Ollama, or a hosted [memcode.ai](https://memcode.ai) account.

## License

MIT. See [LICENSE](LICENSE).
