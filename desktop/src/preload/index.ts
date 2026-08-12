import { contextBridge, ipcRenderer } from 'electron'
import { IPC, type AppInfo, type MenuAction, type StartSessionArgs } from '../shared/ipc'
import type { Attachment, BridgeEvent, PermissionResponseData } from '../shared/protocol'
import type { CatalogModel, SessionRecent, SourcesJSON, StatusJSON } from '../shared/cli-types'

// The only surface the renderer can reach. contextIsolation is on; no Node in
// the renderer. Everything crosses through these typed, allowlisted calls.
const api = {
  startSession: (args: StartSessionArgs): Promise<void> => ipcRenderer.invoke(IPC.sessionStart, args),
  userTurn: (text: string, attachments?: Attachment[]): Promise<string | null> =>
    ipcRenderer.invoke(IPC.sessionUserTurn, text, attachments),
  permission: (id: string, data: PermissionResponseData): Promise<void> =>
    ipcRenderer.invoke(IPC.sessionPermission, id, data),
  ask: (id: string, answer: string): Promise<void> => ipcRenderer.invoke(IPC.sessionAsk, id, answer),
  setModel: (pin: string): Promise<void> => ipcRenderer.invoke(IPC.sessionSetModel, pin),
  cancel: (): Promise<void> => ipcRenderer.invoke(IPC.sessionCancel),
  stop: (): Promise<void> => ipcRenderer.invoke(IPC.sessionStop),

  pickRepo: (): Promise<string | null> => ipcRenderer.invoke(IPC.pickRepo),
  recentRepos: (): Promise<string[]> => ipcRenderer.invoke(IPC.recentRepos),

  status: (): Promise<StatusJSON> => ipcRenderer.invoke(IPC.status),
  models: (pinnableOnly?: boolean): Promise<CatalogModel[]> => ipcRenderer.invoke(IPC.models, pinnableOnly),
  sources: (): Promise<SourcesJSON> => ipcRenderer.invoke(IPC.sources),
  setConfig: (kv: Record<string, string>): Promise<void> => ipcRenderer.invoke(IPC.setConfig, kv),
  sessionsRecent: (cwd: string): Promise<SessionRecent[]> => ipcRenderer.invoke(IPC.sessionsRecent, cwd),
  login: (): Promise<StatusJSON> => ipcRenderer.invoke(IPC.login),
  logout: (): Promise<StatusJSON> => ipcRenderer.invoke(IPC.logout),
  appInfo: (): Promise<AppInfo> => ipcRenderer.invoke(IPC.appInfo),

  /** Subscribe to CLI->app events. Returns an unsubscribe fn. */
  onEvent: (cb: (ev: BridgeEvent) => void): (() => void) => {
    const listener = (_e: unknown, ev: BridgeEvent): void => cb(ev)
    ipcRenderer.on(IPC.bridgeEvent, listener)
    return () => ipcRenderer.removeListener(IPC.bridgeEvent, listener)
  },

  /** Subscribe to native-menu actions. Returns an unsubscribe fn. */
  onMenu: (cb: (action: MenuAction) => void): (() => void) => {
    const listener = (_e: unknown, action: MenuAction): void => cb(action)
    ipcRenderer.on(IPC.menuAction, listener)
    return () => ipcRenderer.removeListener(IPC.menuAction, listener)
  },
}

export type MemcodeApi = typeof api

contextBridge.exposeInMainWorld('memcode', api)
