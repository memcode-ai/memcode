# Memcode Desktop

A native desktop app (macOS, Windows, Linux) over the memcode coding agent. It
drives the same core CLI you use in the terminal — the Electron **main** process
spawns `memcode agent --protocol stream-json` and speaks its NDJSON protocol; the
**renderer** (React) turns that stream into chat, syntax-highlighted diffs,
tool blocks, a plan panel, approval dialogs, and a settings/BYOK surface.

The CLI stays the single source of truth: the model picker reads `memcode models
--json`, auth/version come from `memcode status --json`, and login runs the CLI's
own browser OAuth. Desktop embeds no catalog and stores no separate credentials.

## Architecture

```
renderer (React/TS)  ──IPC──▶  main process           ──stdio──▶  memcode agent
  chat / diffs / dialogs        cli-bridge.ts (NDJSON)             --protocol stream-json
  window.memcode.* (preload)    config.ts (status/models/login)    (built from THIS commit)
```

- `src/main/cli-bridge.ts` — owns one agent subprocess; frames commands, parses
  envelopes, correlates turns, cancels, and shuts down cleanly.
- `src/main/config.ts` — shells the CLI's JSON surface (`status`/`models`) + login.
- `src/preload/index.ts` — the only surface the renderer can reach (contextIsolation).
- `src/shared/protocol.ts` — TS mirror of `internal/wire/streamjson.go` (keep in sync).
- `src/renderer/` — the UI.

## Develop

```bash
cd desktop
pnpm install
# build a core binary the app can find in dev (repo root, one level up):
( cd .. && CGO_ENABLED=0 go build -o memcode . )
pnpm dev
```

`resolve-bin.ts` looks for `../memcode` in dev and `resources/bin/memcode` when
packaged. Set `MEMCODE_AUTO_UPDATE=off` is forced for the bundled binary.

## Package

```bash
pnpm dist        # current OS, via electron-builder
```

Releases are built in CI (`.github/workflows/desktop-release.yml`) on `desktop-v*`
tags: it compiles the core binary from the same commit, bundles it, and
conditionally signs/notarizes when credentials are present (unsigned dev
artifacts otherwise). Desktop uses a separate `desktop-v*` tag namespace so it
never collides with the CLI's `v*` releases.
