// IPC channel names shared by the main process and the preload allowlist.
// renderer -> main are invoke() request/response; main -> renderer is a single
// push channel carrying BridgeEvents.

export const IPC = {
  // session lifecycle (renderer -> main, invoke)
  sessionStart: 'session:start',
  sessionUserTurn: 'session:userTurn',
  sessionPermission: 'session:permission',
  sessionAsk: 'session:ask',
  sessionSetModel: 'session:setModel',
  sessionCancel: 'session:cancel',
  sessionStop: 'session:stop',
  // repo / files
  pickRepo: 'repo:pick',
  recentRepos: 'repo:recent',
  repoInfo: 'repo:info',
  saveAttachment: 'attachment:save',
  // CLI config surface (renderer -> main, invoke)
  status: 'cli:status',
  models: 'cli:models',
  sources: 'cli:sources',
  setConfig: 'cli:setConfig',
  sessionsRecent: 'cli:sessionsRecent',
  login: 'cli:login',
  logout: 'cli:logout',
  appInfo: 'app:info',
  // main -> renderer (push)
  bridgeEvent: 'bridge:event',
  menuAction: 'menu:action',
} as const

// Actions the native application menu dispatches to the renderer.
export type MenuAction = 'login' | 'logout' | 'run-setup' | 'open-settings' | 'new-session' | 'open-folder' | 'toggle-theme'

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

export interface RepoInfo {
  branch: string
  files: number
  added: number
  removed: number
  stagedFiles: number
  stagedAdded: number
  stagedRemoved: number
}

export interface SaveAttachmentArgs {
  name: string
  mime?: string
  bytes: ArrayBuffer
}
