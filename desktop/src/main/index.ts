import { app, BrowserWindow, dialog, ipcMain, shell } from 'electron'
import { join } from 'node:path'
import { CliBridge } from './cli-bridge'
import * as cli from './config'
import { resolveCliBin } from './resolve-bin'
import { initAutoUpdate } from './updater'
import { buildAppMenu } from './menu'
import { recentRepos, recordRepo } from './store'
import { IPC, type AppInfo, type StartSessionArgs } from '../shared/ipc'
import type { Attachment, PermissionResponseData } from '../shared/protocol'

let mainWindow: BrowserWindow | null = null
let bridge: CliBridge | null = null

const binPath = () => resolveCliBin()

function createWindow(): void {
  mainWindow = new BrowserWindow({
    width: 1180,
    height: 820,
    minWidth: 720,
    minHeight: 480,
    titleBarStyle: process.platform === 'darwin' ? 'hiddenInset' : 'default',
    backgroundColor: '#0e0f11',
    show: false,
    webPreferences: {
      preload: join(__dirname, '../preload/index.js'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: false,
    },
  })

  mainWindow.on('ready-to-show', () => mainWindow?.show())

  buildAppMenu(mainWindow)

  // External links open in the system browser, never in-app.
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    shell.openExternal(url)
    return { action: 'deny' }
  })

  if (process.env.ELECTRON_RENDERER_URL) {
    mainWindow.loadURL(process.env.ELECTRON_RENDERER_URL)
  } else {
    mainWindow.loadFile(join(__dirname, '../renderer/index.html'))
  }
}

function forwardBridgeEvents(b: CliBridge): void {
  b.on('event', (ev) => mainWindow?.webContents.send(IPC.bridgeEvent, ev))
}

async function startSession(args: StartSessionArgs): Promise<void> {
  await bridge?.stop()
  bridge = new CliBridge({ binPath: binPath(), cwd: args.cwd, mode: args.mode ?? 'ask', pin: args.pin, resume: args.resume })
  forwardBridgeEvents(bridge)
  bridge.start()
  recordRepo(args.cwd)
}

function registerIpc(): void {
  ipcMain.handle(IPC.sessionStart, (_e, args: StartSessionArgs) => startSession(args))
  ipcMain.handle(IPC.sessionUserTurn, (_e, text: string, attachments?: Attachment[]) => bridge?.userTurn(text, attachments) ?? null)
  ipcMain.handle(IPC.sessionPermission, (_e, id: string, data: PermissionResponseData) => bridge?.permissionResponse(id, data))
  ipcMain.handle(IPC.sessionAsk, (_e, id: string, answer: string) => bridge?.askResponse(id, answer))
  ipcMain.handle(IPC.sessionCancel, () => bridge?.cancel())
  ipcMain.handle(IPC.sessionStop, () => bridge?.stop())

  ipcMain.handle(IPC.pickRepo, async () => {
    const res = await dialog.showOpenDialog(mainWindow!, { properties: ['openDirectory'] })
    return res.canceled ? null : res.filePaths[0]
  })
  ipcMain.handle(IPC.recentRepos, () => recentRepos())

  ipcMain.handle(IPC.status, () => cli.status(binPath()))
  ipcMain.handle(IPC.models, (_e, pinnableOnly?: boolean) => cli.models(binPath(), pinnableOnly))
  ipcMain.handle(IPC.sources, () => cli.sources(binPath()))
  ipcMain.handle(IPC.setConfig, (_e, kv: Record<string, string>) => cli.setConfig(binPath(), kv))
  ipcMain.handle(IPC.sessionsRecent, (_e, cwd: string) => cli.sessionsRecent(binPath(), cwd))
  ipcMain.handle(IPC.login, () => cli.login(binPath()))
  ipcMain.handle(IPC.logout, () => cli.logout(binPath()))
  ipcMain.handle(IPC.appInfo, (): AppInfo => ({
    appVersion: app.getVersion(),
    electron: process.versions.electron,
    platform: `${process.platform}-${process.arch}`,
  }))
}

app.whenReady().then(() => {
  registerIpc()
  createWindow()
  initAutoUpdate()
  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow()
  })
})

app.on('window-all-closed', async () => {
  await bridge?.stop()
  if (process.platform !== 'darwin') app.quit()
})

app.on('before-quit', async () => {
  await bridge?.stop()
})
