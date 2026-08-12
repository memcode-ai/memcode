// TypeScript mirror of the memcode stream-json wire schema
// (internal/wire/streamjson.go). The CLI is the source of truth; keep these in
// sync with that file. One Envelope per line on the subprocess's stdout.

export const PROTOCOL_VERSION = '1'

export interface Envelope {
  version: string
  type: string
  id?: string // correlation id (permission/ask request<->response)
  turn_id?: string
  data?: unknown
}

// client -> CLI
export type ClientMsg =
  | 'initialize'
  | 'user_turn'
  | 'permission_response'
  | 'ask_response'
  | 'cancel'

// CLI -> client
export type ServerMsg =
  | 'initialized'
  | 'assistant_delta'
  | 'tool_call'
  | 'tool_result'
  | 'diff'
  | 'todos'
  | 'permission_request'
  | 'ask_request'
  | 'session_state'
  | 'usage'
  | 'result'
  | 'error'

export type PermissionMode = 'ask' | 'auto' | 'allow-all'

export interface InitializeData {
  cwd?: string
  mode?: PermissionMode
  pin?: string
  resume?: string
}
export interface InitializedData {
  session_id: string
  protocol: string
}
export interface Attachment {
  path: string
  name?: string
  mime?: string
}
export interface UserTurnData {
  text: string
  attachments?: Attachment[]
}
export interface AssistantDeltaData {
  text: string
}
export interface ToolCallData {
  name: string
  target?: string
  detail?: string
}
export interface ToolResultData {
  name: string
  status?: string
}
export interface DiffData {
  path: string
  language?: string
  unified?: string
  added: number
  removed: number
  new_file?: boolean
}
export interface TodoItem {
  text: string
  status: string
}
export interface TodosData {
  items: TodoItem[]
}
export interface PermissionRequestData {
  title: string
  label?: string
  detail?: string
  command?: string
  cwd?: string
  risk?: string
  editable?: boolean
}
export interface PermissionResponseData {
  allow: boolean
  command?: string
  reason?: string
  interrupt?: boolean
  remember?: boolean
  remember_scope?: string
}
export interface AskRequestData {
  question: string
  options?: string[]
}
export interface AskResponseData {
  answer: string
}
export interface UsageData {
  output_tokens: number
}
export interface SessionStateData {
  busy: boolean
  mode?: string
}
export interface ResultData {
  text?: string
  completed: boolean
}
export interface ErrorData {
  message: string
}

// Discriminated event the renderer consumes (server -> renderer over IPC).
export type BridgeEvent =
  | { type: 'initialized'; data: InitializedData }
  | { type: 'assistant_delta'; data: AssistantDeltaData; turnId?: string }
  | { type: 'tool_call'; data: ToolCallData; turnId?: string }
  | { type: 'tool_result'; data: ToolResultData; turnId?: string }
  | { type: 'diff'; data: DiffData; turnId?: string }
  | { type: 'todos'; data: TodosData; turnId?: string }
  | { type: 'permission_request'; id: string; data: PermissionRequestData; turnId?: string }
  | { type: 'ask_request'; id: string; data: AskRequestData; turnId?: string }
  | { type: 'session_state'; data: SessionStateData }
  | { type: 'usage'; data: UsageData }
  | { type: 'result'; data: ResultData; turnId?: string }
  | { type: 'error'; data: ErrorData; turnId?: string }
  | { type: 'bridge_exit'; data: { code: number | null; stderr: string } }
