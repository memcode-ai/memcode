import { existsSync } from 'node:fs'
import { join } from 'node:path'
import { app } from 'electron'

// The packaged app bundles a `memcode` binary built from the SAME commit as the
// app (desktop CI, electron-builder extraResources -> resources/bin). In dev we
// fall back to a locally built binary at the repo root, then to PATH.
export function resolveCliBin(): string {
  const exe = process.platform === 'win32' ? 'memcode.exe' : 'memcode'

  if (app.isPackaged) {
    // extraResources copies bin/ next to the app's resources.
    return join(process.resourcesPath, 'bin', exe)
  }

  // Dev: prefer a binary built at the repo root (two levels up from desktop/).
  const devCandidates = [
    join(app.getAppPath(), '..', exe), // desktop/ -> repo root
    join(app.getAppPath(), '..', '..', exe),
  ]
  for (const c of devCandidates) {
    if (existsSync(c)) return c
  }

  // Last resort: rely on PATH.
  return exe
}
