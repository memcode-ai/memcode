// IPC channel names shared by the main process and the preload allowlist.
// renderer -> main are invoke() request/response; main -> renderer is a single
// push channel carrying BridgeEvents.

export const IPC = {
  // session lifecycle (renderer -> main, invoke)
  sessionStart: 'session:start',
  sessionUserTurn: 'session:userTurn',
  sessionPermission: 'session:permission',
  sessionAsk: 'session:ask',
  sessionCancel: 'session:cancel',
  sessionStop: 'session:stop',
  // repo / files
  pickRepo: 'repo:pick',
  // CLI config surface (renderer -> main, invoke)
  status: 'cli:status',
  models: 'cli:models',
  login: 'cli:login',
  logout: 'cli:logout',
  appInfo: 'app:info',
  // main -> renderer (push)
  bridgeEvent: 'bridge:event',
} as const

export interface StartSessionArgs {
  cwd: string
  mode?: 'ask' | 'auto' | 'allow-all'
  pin?: string
  resume?: string
}

export interface AppInfo {
  appVersion: string
  electron: string
  platform: string
}
