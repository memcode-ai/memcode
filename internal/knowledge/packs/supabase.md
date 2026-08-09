---
name: supabase
triggers: supabase
detect-files: supabase/config.toml, supabase
detect-deps: @supabase/supabase-js, @supabase/ssr
---

## Facts
- Two keys, two trust levels: the **anon/publishable** key is safe for the browser and is subject to Row Level Security; the **service-role** key bypasses RLS entirely and MUST stay server-side only (never in client bundles, never `NEXT_PUBLIC_`).
- RLS is the security boundary. A table with RLS enabled and no policy denies the anon key everything; the service-role key ignores RLS. "It works server-side but 403s in the browser" is almost always a missing/`too-strict` policy, not a bug.
- In Next.js use `@supabase/ssr` (not the old auth-helpers): create separate browser and server clients, and the server client wires Supabase auth to cookies. Auth state lives in cookies, not localStorage, for SSR.
- Schema changes go through migrations in `supabase/migrations/<timestamp>_<name>.sql`, applied in timestamp order — never hand-edit the live schema out of band. The CLI (`supabase migration new`, `supabase db push`) manages them.
- `auth.users` is managed by Supabase; reference it via foreign keys from your own tables rather than duplicating user data.

## Idioms
- Prefer the supabase-js client / SQL migrations over hand-rolled REST/`curl` against the REST endpoint.
- Default new tables to RLS enabled, then add explicit policies for the access you intend; reach for the service-role key only in trusted server code.
- Match the repo's existing client setup (browser vs server helper) instead of instantiating ad-hoc clients.
