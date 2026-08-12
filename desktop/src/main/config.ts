import { execFile } from 'node:child_process'
import { promisify } from 'node:util'

import type { CatalogModel, StatusJSON } from '../shared/cli-types'

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
