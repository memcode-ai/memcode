import { app, Menu, shell, type BrowserWindow, type MenuItemConstructorOptions } from 'electron'
import { IPC, type MenuAction } from '../shared/ipc'

// Native application menu. On macOS this is the top menu bar; on Windows/Linux
// it's the window menu. Account actions (log in/out, run setup) and Settings
// dispatch to the renderer over IPC.menuAction so the UI stays the one place
// that reflects state — main just relays the intent.

export function buildAppMenu(win: BrowserWindow): void {
  const send = (action: MenuAction) => win.webContents.send(IPC.menuAction, action)
  const isMac = process.platform === 'darwin'

  const template: MenuItemConstructorOptions[] = [
    ...(isMac
      ? [
          {
            label: app.name,
            submenu: [
              { role: 'about' as const },
              { type: 'separator' as const },
              { label: 'Settings…', accelerator: 'Cmd+,', click: () => send('open-settings') },
              { type: 'separator' as const },
              { role: 'services' as const },
              { type: 'separator' as const },
              { role: 'hide' as const },
              { role: 'hideOthers' as const },
              { role: 'unhide' as const },
              { type: 'separator' as const },
              { role: 'quit' as const },
            ],
          } satisfies MenuItemConstructorOptions,
        ]
      : []),
    {
      label: 'File',
      submenu: [
        { label: 'New Session', accelerator: 'CmdOrCtrl+N', click: () => send('new-session') },
        ...(isMac ? [] : [{ label: 'Settings…', click: () => send('open-settings') } as MenuItemConstructorOptions]),
        { type: 'separator' },
        isMac ? { role: 'close' } : { role: 'quit' },
      ],
    },
    { label: 'Edit', submenu: [{ role: 'undo' }, { role: 'redo' }, { type: 'separator' }, { role: 'cut' }, { role: 'copy' }, { role: 'paste' }, { role: 'selectAll' }] },
    {
      label: 'Account',
      submenu: [
        { label: 'Log In…', click: () => send('login') },
        { label: 'Log Out', click: () => send('logout') },
        { type: 'separator' },
        { label: 'Run Setup…', click: () => send('run-setup') },
      ],
    },
    { label: 'View', submenu: [{ role: 'reload' }, { role: 'toggleDevTools' }, { type: 'separator' }, { role: 'resetZoom' }, { role: 'zoomIn' }, { role: 'zoomOut' }, { type: 'separator' }, { role: 'togglefullscreen' }] },
    { role: 'window', submenu: [{ role: 'minimize' }, { role: 'zoom' }, ...(isMac ? [{ type: 'separator' as const }, { role: 'front' as const }] : [{ role: 'close' as const }])] },
    {
      role: 'help',
      submenu: [{ label: 'memcode on GitHub', click: () => shell.openExternal('https://github.com/memcode-ai/memcode') }],
    },
  ]

  Menu.setApplicationMenu(Menu.buildFromTemplate(template))
}
