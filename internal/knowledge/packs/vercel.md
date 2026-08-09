---
name: vercel
triggers: vercel
detect-files: vercel.json, .vercel
detect-deps: vercel
---

## Facts
- `VERCEL_ENV` is set automatically by Vercel on every deployment: `"production" | "preview" | "development"`. Read it to branch behavior by environment. NEVER invent a custom `NEXT_PUBLIC_*` flag for "are we in prod" — that reinvents a built-in and has to be set by hand per environment.
- Other auto-set vars: `VERCEL` (`"1"` on Vercel), `VERCEL_URL` (the deployment's generated URL, no protocol), `VERCEL_BRANCH_URL`, `VERCEL_GIT_COMMIT_SHA`, `VERCEL_REGION`.
- `VERCEL_ENV` is `"production"` ONLY for production deployments (the production domain / `vercel --prod`). Every preview branch deploy is `"preview"`. Local `vercel dev` is `"development"`.
- A var must be exposed to the browser to be readable client-side: only `NEXT_PUBLIC_`-prefixed vars reach client bundles. `VERCEL_ENV` is server-side; to use it in the browser you must re-expose it (e.g. map it to a `NEXT_PUBLIC_` var at build) — but prefer reading it server-side.
- Env vars are scoped per-environment (Production / Preview / Development) in the dashboard or via `vercel env add <NAME> <env>`. Adding to one environment does not add it to the others.
- The build fails the deploy: if `next build` (prerender, type, or import errors) fails, the deployment does not publish. A green test run does NOT prove the build — run the build locally before pushing.

## Idioms
- Gate environment-specific behavior server-side on `process.env.VERCEL_ENV`, in a Server Component, route handler, or middleware — not in client code.
- Prefer the built-in vars over custom config whenever one exists; reach for a custom env var only for genuinely app-specific configuration.
