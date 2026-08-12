import { execFile } from 'node:child_process'
import { promisify } from 'node:util'

import { spawn } from 'node:child_process'
import type { CatalogModel, SessionRecent, SourcesJSON, StatusJSON } from '../shared/cli-types'

const pExecFile = promisify(execFile)

// The CLI is the single source of truth for the catalog, auth state, and version.
// These helpers shell out to its machine-readable surface (cmd/gui.go) instead of
// the app embedding its own copy of any of it.

export type { CatalogModel, StatusJSON }

async function runJSON<T>(binPath: string, args: string[], cwd?: string): Promise<T> {
  const { stdout } = await pExecFile(binPath, args, {
    cwd,
    env: { ...process.env, MEMCODE_AUTO_UPDATE: 'off' },
    maxBuffer: 8 * 1024 * 1024,
  })
  return JSON.parse(stdout) as T
}

export function status(binPath: string): Promise<StatusJSON> {
  return runJSON<StatusJSON>(binPath, ['status', '--json'])
}

export function models(binPath: string, pinnableOnly = false): Promise<CatalogModel[]> {
  const args = ['models', '--json']
  if (pinnableOnly) args.push('--pinnable')
  return runJSON<CatalogModel[]>(binPath, args)
}

/** What a first-run wizard needs: backend state + detected subscriptions. */
export function sources(binPath: string): Promise<SourcesJSON> {
  return runJSON<SourcesJSON>(binPath, ['config', 'sources', '--json'])
}

/** Resumable/recent sessions for the open repo (the sidebar). */
export function sessionsRecent(binPath: string, cwd: string): Promise<SessionRecent[]> {
  return runJSON<SessionRecent[]>(binPath, ['session', 'recent', '--json'], cwd).catch(() => [])
}

/**
 * Write backend config (endpoint, subscription source, or a BYOK provider key)
 * via `config set --stdin` — secrets go over stdin, never argv. `kv` keys are
 * env names (e.g. ANTHROPIC_API_KEY, MEMCODE_ENDPOINT_URL, MEMCODE_CREDENTIAL_SOURCE).
 */
export function setConfig(binPath: string, kv: Record<string, string>): Promise<void> {
  return new Promise((resolve, reject) => {
    const proc = spawn(binPath, ['config', 'set', '--stdin'], {
      env: { ...process.env, MEMCODE_AUTO_UPDATE: 'off' },
    })
    let err = ''
    proc.stderr.on('data', (c: Buffer) => (err += c.toString()))
    proc.on('error', reject)
    proc.on('exit', (code) => (code === 0 ? resolve() : reject(new Error(err.trim() || `config set exited ${code}`))))
    for (const [k, v] of Object.entries(kv)) proc.stdin.write(`${k}=${v}\n`)
    proc.stdin.end()
  })
}

/**
 * Launch the CLI's browser OAuth login. `memcode login` opens the system browser,
 * runs the 127.0.0.1 callback, and writes the token to ~/.config/memcode/.env —
 * the app just waits for it to finish and re-reads status.
 */
export async function login(binPath: string): Promise<StatusJSON> {
  await pExecFile(binPath, ['login'], {
    env: { ...process.env, MEMCODE_AUTO_UPDATE: 'off' },
    timeout: 5 * 60 * 1000,
  })
  return status(binPath)
}

export async function logout(binPath: string): Promise<StatusJSON> {
  await pExecFile(binPath, ['logout'], { env: { ...process.env, MEMCODE_AUTO_UPDATE: 'off' } })
  return status(binPath)
}
