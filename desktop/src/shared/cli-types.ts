// Shapes returned by the CLI's machine-readable surface (cmd/gui.go). Shared by
// the main process (which produces them) and the renderer (which displays them).

export interface StatusJSON {
  logged_in: boolean
  token_source: string
  endpoint: string
  version: string
  commit: string
  protocol_version: string
}

export interface Source {
  id: string
  label: string
}

export interface SourcesJSON {
  logged_in: boolean
  has_backend: boolean
  endpoint: string
  credential_source: string
  subscriptions: Source[]
}

export interface SessionRecent {
  id: string
  title: string // generated chat title (falls back to task server-side)
  task: string
  mode: string
  model: string
  files_changed: number
  iterations: number
  resumable: boolean
}

export interface CatalogModel {
  id: string
  label: string
  vendor: string
  name: string
  desc?: string
  window?: number
  vision?: boolean
  pdf?: boolean
  reasoning?: boolean
  pinnable?: boolean
  group?: string
}
