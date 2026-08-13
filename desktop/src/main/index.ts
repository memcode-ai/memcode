import { app, BrowserWindow, dialog, ipcMain, shell } from 'electron'
import { execFile } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { basename, extname, join } from 'node:path'
import { promisify } from 'node:util'
import { CliBridge } from './cli-bridge'
import * as cli from './config'
import { resolveCliBin } from './resolve-bin'
import { initAutoUpdate } from './updater'
import { buildAppMenu } from './menu'
import { recentRepos, recordRepo } from './store'
import { IPC, type AppInfo, type RepoInfo, type SaveAttachmentArgs, type StartSessionArgs } from '../shared/ipc'
import type { Attachment, PermissionResponseData } from '../shared/protocol'

let mainWindow: BrowserWindow | null = null
let bridge: CliBridge | null = null
const execFileAsync = promisify(execFile)

const binPath = () => resolveCliBin()
// The square matrix MEMCODE mark (build/icon.png). Packaged builds get it via
// electron-builder; in dev we set it on the window/dock explicitly.
const iconPath = () => join(app.getAppPath(), 'build', 'icon.png')

function createWindow(): void {
  mainWindow = new BrowserWindow({
    width: 1180,
    height: 820,
    minWidth: 720,
    minHeight: 480,
    titleBarStyle: process.platform === 'darwin' ? 'hiddenInset' : 'default',
    backgroundColor: '#0c0d0f',
    icon: iconPath(),
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

async function git(cwd: string, args: string[]): Promise<string> {
  try {
    const { stdout } = await execFileAsync('git', ['-C', cwd, ...args], { timeout: 2500, maxBuffer: 1024 * 1024 })
    return stdout
  } catch {
    return ''
  }
}

function sumNumstat(out: string): { files: number; added: number; removed: number } {
  let files = 0
  let added = 0
  let removed = 0
  for (const line of out.trimEnd().split('\n')) {
    if (!line) continue
    const [a, r] = line.split(/\s+/, 2)
    files++
    const add = Number.parseInt(a, 10)
    const rem = Number.parseInt(r, 10)
    if (Number.isFinite(add)) added += add
    if (Number.isFinite(rem)) removed += rem
  }
  return { files, added, removed }
}

async function repoInfo(cwd: string): Promise<RepoInfo> {
  const [branchRaw, headRaw, unstagedRaw, stagedRaw, statusRaw] = await Promise.all([
    git(cwd, ['branch', '--show-current']),
    git(cwd, ['rev-parse', '--short', 'HEAD']),
    git(cwd, ['diff', '--numstat']),
    git(cwd, ['diff', '--cached', '--numstat']),
    git(cwd, ['status', '--porcelain']),
  ])
  const unstaged = sumNumstat(unstagedRaw)
  const staged = sumNumstat(stagedRaw)
  const branch = branchRaw.trim() || headRaw.trim() || ''

  for (const line of statusRaw.trimEnd().split('\n')) {
    if (!line.startsWith('??') || line.length < 4) continue
    const path = line.slice(3).trim()
    unstaged.files++
    try {
      const data = await readFile(join(cwd, path), 'utf8')
      unstaged.added += data.split('\n').length - 1
    } catch {
      // Ignore unreadable/binary untracked files; the file count still reflects dirtiness.
    }
  }

  return {
    branch,
    files: unstaged.files,
    added: unstaged.added,
    removed: unstaged.removed,
    stagedFiles: staged.files,
    stagedAdded: staged.added,
    stagedRemoved: staged.removed,
  }
}

function safeAttachmentName(name: string, mime?: string): string {
  const fallback = mime?.startsWith('image/') ? `pasted-image.${mime.split('/')[1] || 'png'}` : 'pasted-text.txt'
  const base = basename(name || fallback)
    .replace(/[^\w .@()+,-]/g, '_')
    .replace(/\s+/g, ' ')
    .trim()
  return base || fallback
}

async function saveAttachment(args: SaveAttachmentArgs): Promise<Attachment> {
  const dir = join(app.getPath('temp'), 'memcode-desktop-attachments')
  await mkdir(dir, { recursive: true })
  const name = safeAttachmentName(args.name, args.mime)
  const ext = extname(name)
  const stem = ext ? name.slice(0, -ext.length) : name
  const path = join(dir, `${stem}-${randomUUID()}${ext}`)
  await writeFile(path, Buffer.from(args.bytes))
  return { path, name, mime: args.mime }
}

function registerIpc(): void {
  ipcMain.handle(IPC.sessionStart, (_e, args: StartSessionArgs) => startSession(args))
  ipcMain.handle(IPC.sessionUserTurn, (_e, text: string, attachments?: Attachment[]) => bridge?.userTurn(text, attachments) ?? null)
  ipcMain.handle(IPC.sessionPermission, (_e, id: string, data: PermissionResponseData) => bridge?.permissionResponse(id, data))
  ipcMain.handle(IPC.sessionAsk, (_e, id: string, answer: string) => bridge?.askResponse(id, answer))
  ipcMain.handle(IPC.sessionSetModel, (_e, pin: string) => bridge?.setModel(pin))
  ipcMain.handle(IPC.sessionCancel, () => bridge?.cancel())
  ipcMain.handle(IPC.sessionStop, () => bridge?.stop())

  ipcMain.handle(IPC.pickRepo, async () => {
    const res = await dialog.showOpenDialog(mainWindow!, { properties: ['openDirectory'] })
    return res.canceled ? null : res.filePaths[0]
  })
  ipcMain.handle(IPC.recentRepos, () => recentRepos())
  ipcMain.handle(IPC.repoInfo, (_e, cwd: string) => repoInfo(cwd))
  ipcMain.handle(IPC.saveAttachment, (_e, args: SaveAttachmentArgs) => saveAttachment(args))

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
  // Dev dock icon on macOS (packaged builds carry the bundle icon already).
  if (process.platform === 'darwin' && !app.isPackaged) {
    try {
      app.dock?.setIcon(iconPath())
    } catch {
      // non-fatal
    }
  }
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
