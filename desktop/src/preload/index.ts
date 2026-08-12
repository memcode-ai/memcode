import { contextBridge, ipcRenderer } from 'electron'
import { IPC, type AppInfo, type StartSessionArgs } from '../shared/ipc'
import type { Attachment, BridgeEvent, PermissionResponseData } from '../shared/protocol'
import type { CatalogModel, StatusJSON } from '../shared/cli-types'

// The only surface the renderer can reach. contextIsolation is on; no Node in
// the renderer. Everything crosses through these typed, allowlisted calls.
const api = {
  startSession: (args: StartSessionArgs): Promise<void> => ipcRenderer.invoke(IPC.sessionStart, args),
  userTurn: (text: string, attachments?: Attachment[]): Promise<string | null> =>
    ipcRenderer.invoke(IPC.sessionUserTurn, text, attachments),
  permission: (id: string, data: PermissionResponseData): Promise<void> =>
    ipcRenderer.invoke(IPC.sessionPermission, id, data),
  ask: (id: string, answer: string): Promise<void> => ipcRenderer.invoke(IPC.sessionAsk, id, answer),
  cancel: (): Promise<void> => ipcRenderer.invoke(IPC.sessionCancel),
  stop: (): Promise<void> => ipcRenderer.invoke(IPC.sessionStop),

  pickRepo: (): Promise<string | null> => ipcRenderer.invoke(IPC.pickRepo),

  status: (): Promise<StatusJSON> => ipcRenderer.invoke(IPC.status),
  models: (pinnableOnly?: boolean): Promise<CatalogModel[]> => ipcRenderer.invoke(IPC.models, pinnableOnly),
  login: (): Promise<StatusJSON> => ipcRenderer.invoke(IPC.login),
  logout: (): Promise<StatusJSON> => ipcRenderer.invoke(IPC.logout),
  appInfo: (): Promise<AppInfo> => ipcRenderer.invoke(IPC.appInfo),

  /** Subscribe to CLI->app events. Returns an unsubscribe fn. */
  onEvent: (cb: (ev: BridgeEvent) => void): (() => void) => {
    const listener = (_e: unknown, ev: BridgeEvent): void => cb(ev)
    ipcRenderer.on(IPC.bridgeEvent, listener)
    return () => ipcRenderer.removeListener(IPC.bridgeEvent, listener)
  },
}

export type MemcodeApi = typeof api

contextBridge.exposeInMainWorld('memcode', api)
