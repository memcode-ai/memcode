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
