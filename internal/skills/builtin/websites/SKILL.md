---
name: websites
description: "Use when the user wants a memcode website ({slug}.memcode.app, the Websites feature): to create, change, preview, publish, or manage one. A plain \"make me a website\" is usually ordinary development in the user's project, not this; ask when it's unclear. Covers telling the two apart, the local-first lifecycle (create → develop → push → publish), the hosted path, what is internal-preview gated, assets, draft vs. live."
metadata:
  version: "4"
  triggers: memcode.app
---

# Websites

Memcode websites are static sites (Vite + React, no TypeScript) that end up
hosted at `{slug}.memcode.app`. In the CLI they work like git: the site is
made and changed **on this machine** with a live local preview, and
memcode.ai only changes when the user crosses that boundary.

| Step | Tool action | CLI | Touches memcode.ai |
| --- | --- | --- | --- |
| Make a site | `create` | `memcode websites init <name>` | no |
| Change it | `develop` (slug, prompt) | `memcode` in its folder | no |
| See it | (develop starts it) | `memcode websites dev <name>` | no |
| Share the code | `zip` | `memcode websites zip <name>` | no |
| Put it online (draft) | `push` | `memcode websites push <name>` | yes: creates it the first time |
| Make it live | `publish` | `memcode websites publish` | yes: what the public sees |

Local copies live in `~/.memcode/websites/<name>/source`, from any
directory. A site is named by its local name until it is pushed, then by its
slug; both work.

## First: is this a memcode website at all?

"Website" is ambiguous. Most people who say "make me a website" are doing
ordinary development: they want files in the project they're in, built with
their own framework, deployed their own way. A memcode website is a separate
product that lives outside their project. Understand which one the user
means before you act, and never assume the memcode one just because you are
memcode.

- **It's a memcode website** when the user names it ("a memcode website",
  "crumb.memcode.app"), is working on one they already have (`list` shows
  it), or chose it when asked.
- **It's ordinary development** when they're in a project that already has a
  site or app, name their own stack or host ("a Next.js landing page", "on
  Vercel"), or talk about files and folders in the repo. Use the file tools
  and don't call this tool.
- **Otherwise, ask.** `create` asks the user itself in an interactive
  session ("In this project" or "As a memcode website"), so don't pre-ask
  the same question; if they choose the project, build it there with the
  file tools. In a one-shot run nobody can answer, so only create a memcode
  website when the request clearly asks for one.

## The default: local

- **"Make me a memcode website for my bakery."** `create` with a name and the
  prompt. The site is made on this machine, the change is made, and a local
  preview starts (the user is asked whether to open it). Nothing exists on
  memcode.ai yet; say so, and that pushing puts it online.
- **"Make the header smaller."** `develop` with the slug and the change.
  The preview updates live; no build, no waiting.
- **"Work on my existing crumb site."** `develop`: a site that is only on
  memcode.ai is pulled into a local copy first; an existing copy is reused
  (and caught up with memcode.ai when it has no changes of its own).
- **"Push it" / "put it online."** `push`. The first push creates the site
  on memcode.ai and builds its draft; later pushes update it. If memcode.ai
  has changes the local copy doesn't (from a hosted build), they are merged
  in first; a conflict stops the push, and you tell the user.
- **"Publish it."** `publish`. If the local copy is ahead, it is pushed and
  built first, so what goes live is what the user saw locally.

Node.js runs the local preview. If it's missing, the user is offered an
install; if they decline, offer the hosted path instead.

## The hosted path: only when asked

When the user asks to build it on memcode.ai ("create it remotely", "build
a hosted preview"), use `create` with `remote:true` and a prompt, `build`
(a prompt runs the hosted agent), or `edit` (verbatim find/replace, no
agent). The status bar shows the build and the user is told when it
finishes. A one-shot `memcode run` has no local preview, so its `create`
builds on memcode.ai.

`preview` offers to open the hosted draft (`preview--{slug}.memcode.app`).
Its link carries a private token: only pass `reveal:true` when the user
explicitly asks for the URL itself. When they want to **share** the preview
with someone, use `share`: a short `memcode.ai/p/...` link anyone can use to
view the draft for 7 days, which can be turned off
(`memcode websites share <slug> --off <link>`).

## What is gated: hosted building is internal-preview only

Anything that **builds on memcode.ai** (`push`, `build`, `edit`, and a
remote `create` with a prompt) is limited to internal accounts right now.
Elsewhere it fails with "website building is in limited internal preview".
That is an **account restriction, not a transient error**: tell the user,
don't retry. Making and developing sites locally, and `zip`, work for
everyone.

## Assets

Uploaded files (`assets_list` / `assets_upload` / `assets_delete`) live
apart from the build output, served publicly at `/assets/<filename>`, and
survive every rebuild. Everything uploaded is **public**: the tool refuses
hidden files and anything that looks like credentials; never work around
that.

## Approvals

Making and changing a site locally asks nothing beyond the usual file
approvals. `push`, `create(remote)`, `build`, `edit` and `update` follow the
approval mode. Anything the public sees or that destroys data (`publish`,
`unpublish`, `delete`, `assets_upload`, `assets_delete`) **always asks**,
even in auto mode. Don't call `delete` unless the user asked for it.
