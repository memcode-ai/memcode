import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { EventEmitter } from 'node:events'
import { createInterface, type Interface } from 'node:readline'
import {
  PROTOCOL_VERSION,
  type Attachment,
  type BridgeEvent,
  type Envelope,
  type InitializeData,
  type PermissionMode,
  type PermissionResponseData,
} from '../shared/protocol'

export interface BridgeOptions {
  /** Absolute path to the bundled `memcode` binary. */
  binPath: string
  /** Working directory the agent operates in (the opened repo). */
  cwd: string
  mode?: PermissionMode
  /** Model pin, or empty/undefined for Automatic. */
  pin?: string
  /** Session id to resume, if any. */
  resume?: string
  /** Extra env injected into the subprocess (token/endpoint overrides). */
  env?: Record<string, string>
}

/**
 * CliBridge owns one `memcode agent --protocol stream-json` subprocess: it frames
 * outbound commands and parses inbound NDJSON envelopes into typed BridgeEvents.
 * Mirrors the builder's agent/session lifecycle. One bridge == one live session;
 * resume starts a fresh subprocess pointed at a prior session id.
 *
 * Emits 'event' (BridgeEvent) for every decoded message and 'exit' when the
 * subprocess ends. The renderer never sees the subprocess — only these events,
 * relayed over IPC.
 */
export class CliBridge extends EventEmitter {
  private proc: ChildProcessWithoutNullStreams | null = null
  private rl: Interface | null = null
  private stderrBuf = ''
  private turnSeq = 0
  private closing = false

  constructor(private readonly opts: BridgeOptions) {
    super()
  }

  /** Spawn the subprocess and send `initialize`. Idempotent guard: no-op if already started. */
  start(): void {
    if (this.proc) return
    this.proc = spawn(this.opts.binPath, ['agent', '--protocol', 'stream-json'], {
      cwd: this.opts.cwd,
      env: {
        ...process.env,
        // Never let the bundled binary self-update from inside the app.
        MEMCODE_AUTO_UPDATE: 'off',
        ...(this.opts.pin ? { MEMCODE_MODEL_PIN: this.opts.pin } : {}),
        ...this.opts.env,
      },
    })

    this.rl = createInterface({ input: this.proc.stdout })
    this.rl.on('line', (line) => this.onLine(line))

    this.proc.stderr.on('data', (chunk: Buffer) => {
      const text = chunk.toString()
      this.stderrBuf += text
      if (this.stderrBuf.length > 64 * 1024) {
        this.stderrBuf = this.stderrBuf.slice(-64 * 1024)
      }
    })

    this.proc.on('exit', (code) => {
      this.rl?.close()
      this.emitEvent({ type: 'bridge_exit', data: { code, stderr: this.stderrBuf } })
      this.emit('exit', code)
      this.proc = null
    })

    this.proc.on('error', (err) => {
      this.emitEvent({ type: 'error', data: { message: `spawn failed: ${err.message}` } })
    })

    const init: InitializeData = {
      cwd: this.opts.cwd,
      mode: this.opts.mode ?? 'ask',
      ...(this.opts.pin ? { pin: this.opts.pin } : {}),
      ...(this.opts.resume ? { resume: this.opts.resume } : {}),
    }
    this.send('initialize', init)
  }

  /** Send a user turn with optional structured attachments (drag-and-drop). */
  userTurn(text: string, attachments?: Attachment[]): string {
    const turnId = `turn_${++this.turnSeq}`
    this.send('user_turn', { text, ...(attachments?.length ? { attachments } : {}) }, undefined, turnId)
    return turnId
  }

  /** Answer a permission_request; id must match the request's id. */
  permissionResponse(id: string, data: PermissionResponseData): void {
    this.send('permission_response', data, id)
  }

  /** Answer an ask_request; id must match the request's id. */
  askResponse(id: string, answer: string): void {
    this.send('ask_response', { answer }, id)
  }

  /** Interrupt the in-flight turn. */
  cancel(): void {
    this.send('cancel', {})
  }

  /**
   * Graceful shutdown: cancel the turn, close stdin so the CLI's read loop ends,
   * then SIGKILL if it hasn't exited within the grace window.
   */
  async stop(graceMs = 2000): Promise<void> {
    if (!this.proc || this.closing) return
    this.closing = true
    try {
      this.cancel()
      this.proc.stdin.end()
    } catch {
      // stdin may already be gone; fall through to the kill timer.
    }
    await new Promise<void>((resolve) => {
      if (!this.proc) return resolve()
      const timer = setTimeout(() => {
        this.proc?.kill('SIGKILL')
        resolve()
      }, graceMs)
      this.proc.once('exit', () => {
        clearTimeout(timer)
        resolve()
      })
    })
  }

  private send(type: string, data: unknown, id?: string, turnId?: string): void {
    if (!this.proc) return
    const env: Envelope = { version: PROTOCOL_VERSION, type, data, ...(id ? { id } : {}), ...(turnId ? { turn_id: turnId } : {}) }
    this.proc.stdin.write(JSON.stringify(env) + '\n')
  }

  private onLine(line: string): void {
    const trimmed = line.trim()
    if (!trimmed) return
    let env: Envelope
    try {
      env = JSON.parse(trimmed) as Envelope
    } catch {
      // A non-JSON line on stdout is a protocol violation; surface it, don't crash.
      this.emitEvent({ type: 'error', data: { message: `unparseable line: ${trimmed.slice(0, 200)}` } })
      return
    }
    this.decode(env)
  }

  // decode maps a raw envelope to a typed BridgeEvent. Unknown types are ignored
  // (forward-compatible: a newer CLI may emit events this build doesn't render).
  private decode(env: Envelope): void {
    const turnId = env.turn_id
    const data = (env.data ?? {}) as never
    switch (env.type) {
      case 'initialized':
        this.emitEvent({ type: 'initialized', data })
        break
      case 'assistant_delta':
        this.emitEvent({ type: 'assistant_delta', data, turnId })
        break
      case 'tool_call':
        this.emitEvent({ type: 'tool_call', data, turnId })
        break
      case 'tool_result':
        this.emitEvent({ type: 'tool_result', data, turnId })
        break
      case 'diff':
        this.emitEvent({ type: 'diff', data, turnId })
        break
      case 'todos':
        this.emitEvent({ type: 'todos', data, turnId })
        break
      case 'permission_request':
        this.emitEvent({ type: 'permission_request', id: env.id ?? '', data, turnId })
        break
      case 'ask_request':
        this.emitEvent({ type: 'ask_request', id: env.id ?? '', data, turnId })
        break
      case 'session_state':
        this.emitEvent({ type: 'session_state', data })
        break
      case 'usage':
        this.emitEvent({ type: 'usage', data })
        break
      case 'result':
        this.emitEvent({ type: 'result', data, turnId })
        break
      case 'error':
        this.emitEvent({ type: 'error', data, turnId })
        break
      default:
        break
    }
  }

  private emitEvent(ev: BridgeEvent): void {
    this.emit('event', ev)
  }
}
