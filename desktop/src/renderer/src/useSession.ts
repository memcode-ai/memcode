import { useEffect, useReducer } from 'react'
import type {
  AskRequestData,
  BridgeEvent,
  DiffData,
  PermissionRequestData,
  TodoItem,
} from '../../shared/protocol'

// The transcript is an ordered list of blocks. Assistant text accumulates into the
// trailing assistant block; tool/diff blocks are appended as they arrive.
export type Block =
  | { kind: 'user'; text: string; attachments: string[] }
  | { kind: 'assistant'; text: string; done: boolean }
  | { kind: 'tool'; name: string; target?: string; detail?: string; output?: string; status: 'running' | 'ok' | 'failed' }
  | { kind: 'diff'; data: DiffData }
  | { kind: 'error'; message: string }

export interface SessionState {
  sessionId: string | null
  blocks: Block[]
  todos: TodoItem[]
  busy: boolean
  mode: string
  inputTokens: number
  tokens: number
  totalOutputTokens: number
  cacheReadTokens: number
  cacheWriteTokens: number
  contextTokens: number
  contextWindow: number
  model: string
  reasoningEffort: string
  servedBy: string
  servedByok: boolean
  runningShells: number
  pendingPermission: (PermissionRequestData & { id: string }) | null
  pendingAsk: (AskRequestData & { id: string }) | null
  exited: boolean
}

const initial: SessionState = {
  sessionId: null,
  blocks: [],
  todos: [],
  busy: false,
  mode: 'ask',
  inputTokens: 0,
  tokens: 0,
  totalOutputTokens: 0,
  cacheReadTokens: 0,
  cacheWriteTokens: 0,
  contextTokens: 0,
  contextWindow: 0,
  model: '',
  reasoningEffort: '',
  servedBy: '',
  servedByok: false,
  runningShells: 0,
  pendingPermission: null,
  pendingAsk: null,
  exited: false,
}

type Action =
  | { kind: 'user'; text: string; attachments: string[] }
  | { kind: 'event'; ev: BridgeEvent }
  | { kind: 'clearPermission' }
  | { kind: 'clearAsk' }
  | { kind: 'reset' }

function reduce(state: SessionState, action: Action): SessionState {
  if (action.kind === 'reset') return initial
  if (action.kind === 'clearPermission') return { ...state, pendingPermission: null }
  if (action.kind === 'clearAsk') return { ...state, pendingAsk: null }
  if (action.kind === 'user') {
    return { ...state, blocks: [...state.blocks, { kind: 'user', text: action.text, attachments: action.attachments }] }
  }
  const ev = action.ev
  switch (ev.type) {
    case 'initialized':
      return { ...state, sessionId: ev.data.session_id, exited: false }
    case 'assistant_delta': {
      const last = state.blocks[state.blocks.length - 1]
      if (last && last.kind === 'assistant' && !last.done) {
        const blocks = state.blocks.slice(0, -1)
        blocks.push({ ...last, text: last.text + ev.data.text })
        return { ...state, blocks }
      }
      return { ...state, blocks: [...state.blocks, { kind: 'assistant', text: ev.data.text, done: false }] }
    }
    case 'tool_call':
      return {
        ...state,
        blocks: [...state.blocks, { kind: 'tool', name: ev.data.name, target: ev.data.target, status: 'running' }],
      }
    case 'tool_result': {
      // Resolve/update the most recent tool block with a matching name.
      const blocks = state.blocks.slice()
      for (let i = blocks.length - 1; i >= 0; i--) {
        const b = blocks[i]
        if (b.kind === 'tool' && b.name === ev.data.name) {
          blocks[i] = {
            ...b,
            status: ev.data.status ? (ev.data.status === 'failed' ? 'failed' : 'ok') : b.status,
            detail: ev.data.detail ?? b.detail,
            output: ev.data.output ?? b.output,
          }
          break
        }
      }
      return { ...state, blocks }
    }
    case 'diff':
      return { ...state, blocks: [...state.blocks, { kind: 'diff', data: ev.data }] }
    case 'todos':
      return { ...state, todos: ev.data.items }
    case 'session_state':
      return { ...state, busy: ev.data.busy, mode: ev.data.mode ?? state.mode }
    case 'usage':
      return {
        ...state,
        inputTokens: ev.data.input_tokens ?? state.inputTokens,
        tokens: ev.data.output_tokens,
        totalOutputTokens: ev.data.total_output_tokens ?? state.totalOutputTokens,
        cacheReadTokens: ev.data.cache_read_tokens ?? state.cacheReadTokens,
        cacheWriteTokens: ev.data.cache_write_tokens ?? state.cacheWriteTokens,
        contextTokens: ev.data.context_tokens ?? state.contextTokens,
        contextWindow: ev.data.context_window ?? state.contextWindow,
        model: ev.data.model ?? state.model,
        reasoningEffort: ev.data.reasoning_effort ?? state.reasoningEffort,
        servedBy: ev.data.served_by ?? state.servedBy,
        servedByok: ev.data.served_byok ?? state.servedByok,
        runningShells: ev.data.running_shells ?? state.runningShells,
      }
    case 'permission_request':
      return { ...state, pendingPermission: { ...ev.data, id: ev.id } }
    case 'ask_request':
      return { ...state, pendingAsk: { ...ev.data, id: ev.id } }
    case 'result': {
      // Finalize the trailing assistant block.
      const last = state.blocks[state.blocks.length - 1]
      if (last && last.kind === 'assistant') {
        const blocks = state.blocks.slice(0, -1)
        blocks.push({ ...last, done: true })
        return { ...state, blocks, busy: false }
      }
      return { ...state, busy: false }
    }
    case 'error':
      return { ...state, blocks: [...state.blocks, { kind: 'error', message: ev.data.message }] }
    case 'bridge_exit':
      return { ...state, busy: false, exited: true }
    default:
      return state
  }
}

export function useSession() {
  const [state, dispatch] = useReducer(reduce, initial)

  useEffect(() => {
    const unsub = window.memcode.onEvent((ev) => dispatch({ kind: 'event', ev }))
    return unsub
  }, [])

  return {
    state,
    // Clear a pending dialog after answering (the CLI emits the next event on its own).
    clearPermission: () => dispatch({ kind: 'clearPermission' }),
    clearAsk: () => dispatch({ kind: 'clearAsk' }),
    recordUserTurn: (text: string, attachments: string[]) => dispatch({ kind: 'user', text, attachments }),
    reset: () => dispatch({ kind: 'reset' }),
  }
}
