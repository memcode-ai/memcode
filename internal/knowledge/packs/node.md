---
name: node
triggers: node, nodejs
detect-files: package.json
detect-deps: node
---

## Facts
- `package.json` `"type": "module"` makes `.js` files ESM (`import`/`export`); without it they are CommonJS (`require`/`module.exports`). Mixing the two in one file is an error; `.mjs` is always ESM, `.cjs` always CommonJS.
- `"engines"` declares supported Node versions; the `"packageManager"` field (e.g. `pnpm@10.33.0`) pins the package manager Corepack/Vercel use — honor it, don't switch managers.
- Dependencies vs devDependencies matters in production installs: anything needed at runtime must be in `dependencies`, since `--production`/deploy installs may prune `devDependencies`.
- Environment variables come from the process environment (`process.env.X`), not from `.env` automatically — Node does not load `.env` unless something (the framework, `--env-file`, or `dotenv`) loads it. `.env*` files are typically gitignored; `.env.example` documents the keys.
- Lockfiles (`pnpm-lock.yaml` / `package-lock.json` / `yarn.lock`) are the source of truth for installs and must be committed; an out-of-sync lockfile fails CI installs (`--frozen-lockfile`).

## Idioms
- Match the repo's existing package manager and module system; never introduce a second one.
- Keep secrets out of committed files; read them from `process.env` and document them in `.env.example`.
