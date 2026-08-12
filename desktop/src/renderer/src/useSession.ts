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
  | { kind: 'tool'; name: string; target?: string; status: 'running' | 'ok' | 'failed' }
  | { kind: 'diff'; data: DiffData }
  | { kind: 'error'; message: string }

export interface SessionState {
  sessionId: string | null
  blocks: Block[]
  todos: TodoItem[]
  busy: boolean
  mode: string
  tokens: number
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
  tokens: 0,
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
      // Resolve the most recent running tool block with a matching name.
      const blocks = state.blocks.slice()
      for (let i = blocks.length - 1; i >= 0; i--) {
        const b = blocks[i]
        if (b.kind === 'tool' && b.status === 'running' && b.name === ev.data.name) {
          blocks[i] = { ...b, status: ev.data.status === 'failed' ? 'failed' : 'ok' }
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
      return { ...state, tokens: ev.data.output_tokens }
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
