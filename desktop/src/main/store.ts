import { app } from 'electron'
import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

// Tiny persisted UI state in the app's userData dir — currently the list of
// repos you've opened, most-recent first. Lets the app reopen your last repo on
// launch instead of starting cold at the picker every time.
interface State {
  recentRepos: string[]
}

const file = (): string => join(app.getPath('userData'), 'memcode-desktop.json')

function read(): State {
  try {
    return JSON.parse(readFileSync(file(), 'utf8')) as State
  } catch {
    return { recentRepos: [] }
  }
}

function write(s: State): void {
  try {
    writeFileSync(file(), JSON.stringify(s, null, 2))
  } catch {
    // best-effort; a missing userData dir or read-only fs just means no persistence.
  }
}

/** Recent repos that still exist on disk, most-recent first. */
export function recentRepos(): string[] {
  return read().recentRepos.filter((r) => existsSync(r))
}

/** Record a repo as most-recently-opened (deduped, capped). */
export function recordRepo(dir: string): void {
  const s = read()
  s.recentRepos = [dir, ...s.recentRepos.filter((r) => r !== dir)].slice(0, 10)
  write(s)
}
