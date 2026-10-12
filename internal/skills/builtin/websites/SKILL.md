---
name: websites
description: "Use when the user wants to create, inspect, build, edit, preview, publish, or manage an AI-built static website HOSTED BY memcode.ai ({slug}.memcode.app, the Websites feature) — or work on one locally (pull/dev/push). Not for other websites: a site in this repo or hosted elsewhere is ordinary file work. Covers the lifecycle, what is internal-preview gated, assets, draft vs. live, publish/unpublish, delete."
metadata:
  version: "2"
  triggers: memcode.app
---

# Websites

memcode.ai hosts AI-built static sites (Vite + React, no TypeScript):
prompt → build → preview → publish to `{slug}.memcode.app`. There are two
surfaces over the same API:

- **The `websites` tool**: remote actions by slug (list, get, create,
  build, edit, builds, preview, publish, unpublish, update, delete, assets).
  One call each, no shell.
- **The `memcode websites` CLI** (via bash): anything needing real local
  files: `pull` (clone the site's repo), `push` (upload changes and
  rebuild), `dev` (the actual Vite dev server). Inside a pulled checkout,
  plain `memcode` is website mode: the same doctrine the hosted builder
  uses.

Sites are addressed by **slug**. Resolve it with `list`/`get`; never guess
ids. A pulled checkout records its site in `.memcode/website.json`.

## What is gated: building is internal-preview only

Anything that **builds** a site is limited to internal accounts
(`canSeeTestPlans`) right now:

- the tool's `build` and `edit`, and `create` when given a prompt;
- `memcode websites build` and `memcode websites push` (a push rebuilds).

For any other account these fail with "website building is in limited
internal preview". That is an **account restriction, not a transient
error**:

1. Tell the user plainly that hosted building isn't available on their
   account yet.
2. **Don't retry, and don't invent a workaround.** In particular, there
   is no local fallback for a site that was never built: a site only
   has a repo to `pull` once a build has run.

Not gated: `list`, `get`, `create` (without a build), `builds`, `preview`,
`update`, `publish`/`unpublish` of an already-built site, `delete`,
`assets_*`, and `pull` of a site that has been built.

## Lifecycle

1. **Create**: `websites{action:"create", name, prompt}` or
   `memcode websites create "Name" --prompt "…"`. Creating always
   succeeds. With a prompt it also queues a build, which is subject to
   the gate above; if the build is refused, the site exists but is empty.
2. **Build / change** (internal accounts):
   - `build` with a prompt runs the hosted agent; with no prompt it
     rebuilds the current state.
   - `edit` applies verbatim find/replace edits with no agent; fastest
     for small text tweaks.
   - Hands-on: `memcode websites pull <slug>`, edit with the normal
     tools (or run `memcode` inside it), then `memcode websites push`.
   - Check progress with `builds`.
3. **Preview**: the draft lives at `preview--{slug}.memcode.app` (tool
   `preview`, or `memcode websites open <slug> --preview`), token-gated and
   separate from the live site. Check work there before publishing.
4. **Publish / unpublish**: `publish` promotes the current draft to
   `{slug}.memcode.app`; `unpublish` takes it offline without deleting.
   Nothing is live until published.

## Running it locally: `memcode websites dev`

`memcode websites dev <slug>` runs the site's real Vite dev server (full
HMR, real devtools). With a slug it uses (or first clones into) a managed
workspace under `~/.memcode/websites/<id>`; inside a pulled checkout, run it
with no argument. Like `pull`, it needs a site that has been built. **It's
long-running: launch it with bash `background:true`** and read its output
for the local URL.

## Assets

Uploaded files (`assets_list` / `assets_upload` / `assets_delete`, or
`memcode websites assets …`) live in a durable prefix served publicly at
`/assets/<filename>`, separate from the build output, so they survive every
rebuild. Use it for files the user wants attached permanently (a PDF, a
logo). Everything uploaded is **public**: the tool refuses hidden files and
anything that looks like credentials, and you should never try to work
around that.

## Approvals

Draft and metadata changes (`create`, `build`, `edit`, `update`) follow the
normal approval mode. Anything the public sees or that destroys data
(`publish`, `unpublish`, `delete`, `assets_upload`, `assets_delete`)
**always asks**, even in auto mode, and can't be pre-approved. Don't call
`delete` unless the user asked for it.

## Worked scenarios

**"Make me a website for my coffee shop."** `create` with a descriptive
name and prompt. If the build queued: watch `builds`, then share the
`preview` URL and offer to `publish`. If the build was refused by the
gate: say the site was created but hosted building isn't available on
their account yet. Stop there; don't retry.

**"Work on my X website."** Resolve the slug. For small text changes,
`edit`. For anything bigger: `memcode websites pull <slug>`, make the
changes with the normal tools, `memcode websites push`, then share the
preview. If `pull` says the site has never been built, there is nothing to
work on locally yet.

**"Add this PDF to my site."** Resolve the slug, `assets_upload` the file,
and give the user its `/assets/<name>` URL, noting it survives rebuilds and
can be linked from the site's pages.
