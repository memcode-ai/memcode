// Theme handling: light | dark | system. 'system' leaves no data-theme attribute
// so the CSS prefers-color-scheme media query decides; an explicit choice stamps
// data-theme on <html> and wins over the OS. Persisted in localStorage.

export type Theme = 'light' | 'dark' | 'system'

const KEY = 'memcode-theme'

export function getTheme(): Theme {
  const v = localStorage.getItem(KEY)
  return v === 'light' || v === 'dark' ? v : 'system'
}

export function applyTheme(t: Theme): void {
  const root = document.documentElement
  if (t === 'system') root.removeAttribute('data-theme')
  else root.setAttribute('data-theme', t)
  localStorage.setItem(KEY, t)
}

/** Whether the app is rendering dark right now (explicit or via the OS). */
export function isDark(): boolean {
  const t = getTheme()
  if (t === 'dark') return true
  if (t === 'light') return false
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

/** Flip between light and dark (resolving 'system' to its current effective value first). */
export function toggleTheme(): Theme {
  const next: Theme = isDark() ? 'light' : 'dark'
  applyTheme(next)
  return next
}

// Apply the stored choice as early as possible.
applyTheme(getTheme())
