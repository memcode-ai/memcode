---
name: next
triggers: next, nextjs
detect-files: next.config.js, next.config.mjs, next.config.ts
detect-deps: next
---

## Facts
- App Router components are **Server Components by default**. A file is a Client Component only when it starts with the `'use client'` directive at the very top.
- Client-only APIs are forbidden in Server Components: React hooks (`useState`/`useEffect`/`useRef`), browser APIs (`window`/`document`), event handlers (`onClick`), and **any library that uses them** (e.g. `framer-motion`/`motion`, most chart/animation libs). Using one without `'use client'` fails the build at prerender — "Export encountered an error on /…". If you strip `'use client'` from a file that still uses motion/hooks, the build breaks.
- A Server Component MAY import and render a Client Component (that's the normal composition). The directive belongs on the file that actually uses the client API, not necessarily the page.
- `process.env.NEXT_PUBLIC_*` vars are inlined into the client bundle at **build time**; all other env vars are server-only. A non-prefixed var read in client code is `undefined`.
- Route handlers live at `app/**/route.{js,ts}` and export HTTP-verb functions (`GET`, `POST`, …). Pages live at `app/**/page.{jsx,tsx}`. `layout` wraps a segment; metadata is exported, not rendered.
- `next build` runs static generation/prerender for static routes — runtime errors in a page surface there, not just in tests.

## Idioms
- Keep `'use client'` as low in the tree as possible — push interactivity into small leaf Client Components and keep pages/layouts as Server Components for data fetching.
- When removing interactive UI (a form, animations) from a component, check whether the `'use client'` directive and its imports are still needed — and whether the file still needs to be a client component at all.
- Co-locate data fetching in Server Components; avoid client-side fetch waterfalls for initial render.
