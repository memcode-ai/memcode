import { autoUpdater } from 'electron-updater'
import { app } from 'electron'

// Auto-update against GitHub Releases (desktop-v* tags; see electron-builder.yml).
// No-op in dev and when packaged without an update feed.
export function initAutoUpdate(): void {
  if (!app.isPackaged) return
  autoUpdater.autoDownload = true
  autoUpdater.on('error', (err) => console.error('auto-update error:', err.message))
  autoUpdater.checkForUpdatesAndNotify().catch(() => {})
}
