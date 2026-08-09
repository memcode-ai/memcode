# Self-hosting memcode

The memcode gateway is a single stateless binary. Self-hosting means running
it with your own provider keys; the CLI then works exactly as it does against
the hosted service, minus the account features (one shared balance, the BYOK
key vault, hosted web search billing).

## Quick start

```bash
git clone https://github.com/memcode-ai/memcode
cd memcode/deploy/docker
cp ../../.example.env .env
# edit .env: set MEMCODE_SELFHOST_TOKEN (any strong random string)
# and at least one provider key
docker compose up --build
```

Then, in any repo:

```bash
MEMCODE_API_URL=http://localhost:8080 \
MEMCODE_API_TOKEN=<your MEMCODE_SELFHOST_TOKEN> \
memcode
```

## What self-host mode is

Setting `MEMCODE_SELFHOST_TOKEN` puts the gateway in self-host mode: every
request carrying that bearer runs as a single always-entitled local identity.
No accounts, no billing, no outbound calls to memcode.ai. Without it the
open-source gateway has no authenticator at all and answers every request
with a 500 — set the token (or inject an Authenticator when embedding the
serving core) before pointing a CLI at it.

The managed key-vault endpoints (/v1/byok/*) don't exist in the self-host
gateway at all — they're part of the hosted service (Memcode Cloud), which
composes them privately on top of this same open-source core. Self-hosting,
your provider keys come from the environment, and usage logs as JSON lines
on stdout.

## Without docker

```bash
go build -o memcode-gateway ./gateway/cmd/memcode-gateway
MEMCODE_SELFHOST_TOKEN=... ANTHROPIC_API_KEY=... ./memcode-gateway
```

## No gateway at all

The CLI also runs with no gateway anywhere: point it straight at a provider
or a local model and it keeps full routing policy client-side.

```bash
MEMCODE_ENDPOINT_URL=http://localhost:11434/v1 memcode   # Ollama
MEMCODE_ENDPOINT_URL=https://api.openai.com/v1 memcode   # your OpenAI key
```

## The web app

The memcode.ai web app (accounts, billing, dashboard) is the hosted
service's control plane and is not part of this repository. Self-hosting
never needs it: the agent and the gateway above are the whole product.
