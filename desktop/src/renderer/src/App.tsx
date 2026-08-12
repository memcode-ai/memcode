import { useCallback, useEffect, useRef, useState } from 'react'
import { useSession, type Block } from './useSession'
import { SetupWizard } from './SetupWizard'
import { SessionSidebar } from './SessionSidebar'
import { MatrixWordmark } from './MatrixWordmark'
import { toggleTheme } from './theme'
import type { CatalogModel, StatusJSON } from '../../shared/cli-types'
import type { Attachment, DiffData } from '../../shared/protocol'
import type { AppInfo, MenuAction } from '../../shared/ipc'

export default function App() {
  const { state, clearPermission, clearAsk, recordUserTurn, reset } = useSession()
  const [repo, setRepo] = useState<string | null>(null)
  const [sessionLive, setSessionLive] = useState(false) // a CLI session is spawned for `repo`
  const [models, setModels] = useState<CatalogModel[]>([])
  const [pin, setPin] = useState('') // '' = Automatic
  const [status, setStatus] = useState<StatusJSON | null>(null)
  const [appInfo, setAppInfo] = useState<AppInfo | null>(null)
  const [showSettings, setShowSettings] = useState(false)
  const [showWizard, setShowWizard] = useState(false)
  const [sidebarKey, setSidebarKey] = useState(0)
  const [recents, setRecents] = useState<string[]>([])
  const bootedRef = useRef(false)

  const refreshStatus = useCallback(() => window.memcode.status().then(setStatus).catch(() => {}), [])

  useEffect(() => {
    window.memcode.models(true).then(setModels).catch(() => {})
    window.memcode.appInfo().then(setAppInfo).catch(() => {})
    refreshStatus()
    window.memcode
      .sources()
      .then((s) => setShowWizard(!s.has_backend))
      .catch(() => {})
  }, [refreshStatus])

  const doLogin = useCallback(async () => {
    try {
      await window.memcode.login()
    } finally {
      refreshStatus()
    }
  }, [refreshStatus])

  const doLogout = useCallback(async () => {
    try {
      await window.memcode.logout()
    } finally {
      refreshStatus()
    }
  }, [refreshStatus])

  // Selecting a folder loads the project (sidebar sessions) but does NOT spawn a
  // CLI session — that happens lazily on the first message, so the app lands on
  // the branded empty state instead of a blank chat.
  const selectRepo = useCallback(
    (dir: string) => {
      setRepo(dir)
      setSessionLive(false)
      reset()
      setSidebarKey((k) => k + 1)
    },
    [reset],
  )

  const openRepo = useCallback(async () => {
    const dir = await window.memcode.pickRepo()
    if (!dir) return
    selectRepo(dir)
  }, [selectRepo])

  const newSession = useCallback(async () => {
    await window.memcode.stop()
    setSessionLive(false)
    reset()
  }, [reset])

  // Resume a conversation, switching to its project first if needed.
  const resumeIn = useCallback(
    async (dir: string, id: string) => {
      setRepo(dir)
      reset()
      await window.memcode.startSession({ cwd: dir, mode: 'ask', pin: pin || undefined, resume: id })
      setSessionLive(true)
      setSidebarKey((k) => k + 1)
    },
    [pin, reset],
  )

  // Spawn the CLI session on demand (first message of a fresh project).
  const ensureSession = useCallback(async () => {
    if (!repo || sessionLive) return
    await window.memcode.startSession({ cwd: repo, mode: 'ask', pin: pin || undefined })
    setSessionLive(true)
  }, [repo, sessionLive, pin])

  const send = useCallback(
    async (text: string, attachments: Attachment[]) => {
      if (!repo || !text.trim()) return
      await ensureSession()
      recordUserTurn(text, attachments.map((a) => a.name ?? a.path))
      await window.memcode.userTurn(text, attachments.length ? attachments : undefined)
    },
    [repo, ensureSession, recordUserTurn],
  )

  // On launch, reselect the last project (populates the sidebar) but stay on the
  // empty state — no session, no forced chat — until you type something.
  useEffect(() => {
    window.memcode
      .recentRepos()
      .then((r) => {
        setRecents(r)
        if (!bootedRef.current && r.length > 0) {
          bootedRef.current = true
          selectRepo(r[0])
        }
      })
      .catch(() => {})
  }, [selectRepo])

  // Native menu actions.
  useEffect(() => {
    return window.memcode.onMenu((action: MenuAction) => {
      switch (action) {
        case 'login':
          doLogin()
          break
        case 'logout':
          doLogout()
          break
        case 'run-setup':
          setShowWizard(true)
          break
        case 'open-settings':
          setShowSettings(true)
          break
        case 'new-session':
          newSession()
          break
        case 'open-folder':
          openRepo()
          break
        case 'toggle-theme':
          toggleTheme()
          break
      }
    })
  }, [doLogin, doLogout, newSession, openRepo])

  // Refetch the session sidebar whenever a turn finishes (a new/updated transcript).
  useEffect(() => {
    if (!state.busy) setSidebarKey((k) => k + 1)
  }, [state.busy])

  return (
    <div className="app">
      <div className="body">
        <SessionSidebar
          projects={repo ? [repo, ...recents.filter((r) => r !== repo)] : recents}
          activeRepo={repo}
          activeId={state.sessionId}
          reloadKey={sidebarKey}
          onNew={newSession}
          onResume={resumeIn}
          onSelectProject={selectRepo}
        />
        <div className="content">
          <div className="drag-strip" />
          <main className="main">
            {state.blocks.length === 0 ? (
              <EmptyState repo={repo} recents={recents} onOpenRecent={selectRepo} loggedIn={status?.logged_in ?? false} />
            ) : (
              <Transcript blocks={state.blocks} />
            )}
          </main>
          <Composer
            noRepo={!repo}
            models={models}
            pin={pin}
            onPin={setPin}
            disabled={state.exited}
            busy={state.busy}
            onSend={send}
            onCancel={() => window.memcode.cancel()}
          />
        </div>
        {state.todos.length > 0 && <PlanPanel todos={state.todos} />}
      </div>

      <StatusBar status={status} tokens={state.tokens} mode={state.mode} busy={state.busy} sessionId={state.sessionId} />

      {state.pendingPermission && (
        <PermissionDialog
          req={state.pendingPermission}
          onDone={(data) => {
            window.memcode.permission(state.pendingPermission!.id, data)
            clearPermission()
          }}
        />
      )}
      {state.pendingAsk && (
        <AskDialog
          req={state.pendingAsk}
          onAnswer={(answer) => {
            window.memcode.ask(state.pendingAsk!.id, answer)
            clearAsk()
          }}
        />
      )}
      {showSettings && (
        <Settings
          status={status}
          appInfo={appInfo}
          onClose={() => setShowSettings(false)}
          onChanged={refreshStatus}
          onLogin={doLogin}
          onLogout={doLogout}
        />
      )}
      {showWizard && (
        <SetupWizard
          onDone={() => {
            setShowWizard(false)
            refreshStatus()
          }}
        />
      )}
    </div>
  )
}

function EmptyState(props: {
  repo: string | null
  recents: string[]
  onOpenRecent: (dir: string) => void
  loggedIn: boolean
}) {
  return (
    <div className="empty">
      <MatrixWordmark className="watermark-canvas" />
      {props.repo ? (
        <p className="empty-tag">Tell me the goal — I&apos;ll search the repo, edit files, run tests, and explain as I go.</p>
      ) : (
        <>
          <p className="empty-tag">
            Open a folder to start — from the toolbar, or <kbd>⌘O</kbd>.
          </p>
          {props.recents.length > 0 && (
            <div className="recents">
              <div className="recents-title">Recent</div>
              {props.recents.map((r) => (
                <button key={r} className="recent-item" title={r} onClick={() => props.onOpenRecent(r)}>
                  <span className="recent-text">
                    <span className="recent-name">{r.split('/').pop()}</span>
                    <span className="recent-path">{r}</span>
                  </span>
                </button>
              ))}
            </div>
          )}
          {!props.loggedIn && <p className="hint">Not signed in — open Settings to log in or set a BYOK key.</p>}
        </>
      )}
    </div>
  )
}

function Transcript({ blocks }: { blocks: Block[] }) {
  const endRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    endRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [blocks])
  return (
    <div className="transcript">
      {blocks.map((b, i) => (
        <BlockView key={i} block={b} />
      ))}
      <div ref={endRef} />
    </div>
  )
}

function BlockView({ block }: { block: Block }) {
  switch (block.kind) {
    case 'user':
      return (
        <div className="msg user">
          <div className="msg-text">{block.text}</div>
          {block.attachments.length > 0 && (
            <div className="chips">
              {block.attachments.map((a, i) => (
                <span className="chip" key={i}>
                  📎 {a}
                </span>
              ))}
            </div>
          )}
        </div>
      )
    case 'assistant':
      return (
        <div className="msg assistant">
          <div className="msg-text">{block.text}</div>
          {!block.done && <span className="cursor">▋</span>}
        </div>
      )
    case 'tool':
      return (
        <div className={`tool tool-${block.status}`}>
          <span className="tool-glyph">{block.status === 'failed' ? '✗' : block.status === 'ok' ? '⏺' : '○'}</span>
          <span className="tool-name">{block.name}</span>
          {block.target && <span className="tool-target">{block.target}</span>}
        </div>
      )
    case 'diff':
      return <DiffView data={block.data} />
    case 'error':
      return <div className="msg error">{block.message}</div>
  }
}

function DiffView({ data }: { data: DiffData }) {
  const [open, setOpen] = useState(true)
  const name = data.path.split('/').pop()
  return (
    <div className="diff">
      <button className="diff-head" onClick={() => setOpen(!open)}>
        <span className="diff-glyph">{data.new_file ? '＋' : '✎'}</span>
        <span className="diff-path">{name}</span>
        <span className="diff-stat">
          <span className="add">+{data.added}</span> <span className="del">−{data.removed}</span>
        </span>
      </button>
      {open && data.unified && (
        <pre className="diff-body">
          {data.unified.split('\n').map((line, i) => (
            <div key={i} className={diffLineClass(line)}>
              {line || ' '}
            </div>
          ))}
        </pre>
      )}
    </div>
  )
}

function diffLineClass(line: string): string {
  if (line.startsWith('+') && !line.startsWith('+++')) return 'dl add'
  if (line.startsWith('-') && !line.startsWith('---')) return 'dl del'
  if (line.startsWith('@@')) return 'dl hunk'
  return 'dl'
}

function PlanPanel({ todos }: { todos: { text: string; status: string }[] }) {
  const glyph = (s: string) =>
    s === 'done' ? '✓' : s === 'in_progress' ? '◐' : s === 'blocked' ? '⊘' : s === 'skipped' ? '⤼' : '○'
  return (
    <aside className="plan">
      <div className="plan-title">Plan</div>
      {todos.map((t, i) => (
        <div key={i} className={`plan-item s-${t.status}`}>
          <span className="plan-glyph">{glyph(t.status)}</span>
          {t.text}
        </div>
      ))}
    </aside>
  )
}

function Composer(props: {
  noRepo: boolean
  models: CatalogModel[]
  pin: string
  onPin: (v: string) => void
  disabled: boolean
  busy: boolean
  onSend: (text: string, attachments: Attachment[]) => void
  onCancel: () => void
}) {
  const [text, setText] = useState('')
  const [atts, setAtts] = useState<Attachment[]>([])
  const [dragging, setDragging] = useState(false)
  const taRef = useRef<HTMLTextAreaElement>(null)

  const grow = () => {
    const ta = taRef.current
    if (!ta) return
    ta.style.height = 'auto'
    ta.style.height = `${Math.min(ta.scrollHeight, 240)}px`
  }

  const submit = () => {
    if (!text.trim()) return
    props.onSend(text, atts)
    setText('')
    setAtts([])
    requestAnimationFrame(grow)
  }

  const onDrop = (e: React.DragEvent) => {
    e.preventDefault()
    setDragging(false)
    const next: Attachment[] = []
    for (const f of Array.from(e.dataTransfer.files)) {
      // Electron exposes the absolute path on dropped File objects.
      const path = (f as File & { path?: string }).path
      if (path) next.push({ path, name: f.name, mime: f.type || undefined })
    }
    if (next.length) setAtts((prev) => [...prev, ...next])
  }

  return (
    <div className="composer-wrap">
      <div
        className={`composer ${dragging ? 'dragging' : ''} ${props.noRepo ? 'disabled' : ''}`}
        onDragOver={(e) => {
          if (props.noRepo) return
          e.preventDefault()
          setDragging(true)
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={onDrop}
      >
        {atts.length > 0 && (
          <div className="chips">
            {atts.map((a, i) => (
              <span className="chip" key={i}>
                📎 {a.name}
                <button className="chip-x" onClick={() => setAtts(atts.filter((_, j) => j !== i))}>
                  ×
                </button>
              </span>
            ))}
          </div>
        )}
        <textarea
          ref={taRef}
          className="input"
          placeholder={
            props.noRepo
              ? 'Open a folder to start a session…'
              : dragging
                ? 'Drop files to attach…'
                : 'Ask memcode to build, fix, or explain…'
          }
          value={text}
          disabled={props.disabled || props.noRepo}
          rows={1}
          onChange={(e) => {
            setText(e.target.value)
            grow()
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit()
          }}
        />
        <div className="composer-row">
          <span className="attach-btn" title="Drag files onto the composer to attach">
            +
          </span>
          <select
            className="model model-mini"
            value={props.pin}
            onChange={(e) => props.onPin(e.target.value)}
            title="Model"
          >
            <option value="">Automatic</option>
            {props.models.map((m) => (
              <option key={m.id} value={m.id}>
                {m.label}
              </option>
            ))}
          </select>
          <span className="composer-hint">{props.noRepo ? 'No folder open' : '⌘↵ to send'}</span>
          {props.busy ? (
            <button className="stop-btn" onClick={props.onCancel} title="Stop">
              ■
            </button>
          ) : (
            <button className="send-btn" onClick={submit} disabled={props.noRepo || props.disabled || !text.trim()} title="Send">
              ↑
            </button>
          )}
        </div>
      </div>
    </div>
  )
}

function PermissionDialog(props: {
  req: { id: string; title: string; label?: string; detail?: string; command?: string; cwd?: string; risk?: string; editable?: boolean }
  onDone: (data: {
    allow: boolean
    command?: string
    reason?: string
    interrupt?: boolean
    remember?: boolean
    remember_scope?: string
  }) => void
}) {
  const { req } = props
  const [command, setCommand] = useState(req.command ?? '')
  const [remember, setRemember] = useState(false)
  return (
    <Modal>
      <div className={`dialog risk-${req.risk ?? 'normal'}`}>
        <h3>{req.title}</h3>
        {req.detail && <p className="detail">{req.detail}</p>}
        {req.command !== undefined && (
          <textarea className="cmd" value={command} readOnly={!req.editable} onChange={(e) => setCommand(e.target.value)} />
        )}
        {req.cwd && <div className="cwd">in {req.cwd}</div>}
        <label className="remember">
          <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)} /> Don't ask again for this
        </label>
        <div className="dialog-actions">
          <button className="ghost" onClick={() => props.onDone({ allow: false, interrupt: true })}>
            Interrupt
          </button>
          <button className="ghost" onClick={() => props.onDone({ allow: false, reason: 'denied' })}>
            Deny
          </button>
          <button
            className="primary"
            onClick={() =>
              props.onDone({
                allow: true,
                command: req.editable ? command : undefined,
                remember,
                remember_scope: remember ? 'project' : undefined,
              })
            }
          >
            {req.label ?? 'Allow'}
          </button>
        </div>
      </div>
    </Modal>
  )
}

function AskDialog(props: { req: { id: string; question: string; options?: string[] }; onAnswer: (a: string) => void }) {
  const [free, setFree] = useState('')
  return (
    <Modal>
      <div className="dialog">
        <h3>{props.req.question}</h3>
        {props.req.options?.length ? (
          <div className="ask-options">
            {props.req.options.map((o) => (
              <button key={o} className="ghost" onClick={() => props.onAnswer(o)}>
                {o}
              </button>
            ))}
          </div>
        ) : (
          <div className="dialog-actions">
            <input className="cmd" value={free} onChange={(e) => setFree(e.target.value)} autoFocus />
            <button className="primary" onClick={() => props.onAnswer(free)}>
              Answer
            </button>
          </div>
        )}
      </div>
    </Modal>
  )
}

function Settings(props: {
  status: StatusJSON | null
  appInfo: AppInfo | null
  onClose: () => void
  onChanged: () => void
  onLogin: () => Promise<void>
  onLogout: () => Promise<void>
}) {
  const [busy, setBusy] = useState(false)
  const [key, setKey] = useState('')
  const [endpoint, setEndpoint] = useState('')
  const [saved, setSaved] = useState('')

  const withBusy = async (fn: () => Promise<unknown>) => {
    setBusy(true)
    setSaved('')
    try {
      await fn()
    } finally {
      setBusy(false)
    }
  }
  const doLogin = () => withBusy(props.onLogin)
  const doLogout = () => withBusy(props.onLogout)
  const saveKey = () =>
    withBusy(async () => {
      const v = key.trim()
      if (!v) return
      const env = v.startsWith('sk-ant-') ? 'ANTHROPIC_API_KEY' : 'OPENAI_API_KEY'
      await window.memcode.setConfig({ [env]: v })
      setKey('')
      setSaved(`Saved ${env}`)
      props.onChanged()
    })
  const saveEndpoint = () =>
    withBusy(async () => {
      const v = endpoint.trim()
      if (!v) return
      await window.memcode.setConfig({ MEMCODE_ENDPOINT_URL: v })
      setEndpoint('')
      setSaved('Saved endpoint')
      props.onChanged()
    })

  const s = props.status
  return (
    <Modal>
      <div className="dialog settings">
        <div className="settings-head">
          <h3>Settings</h3>
          <button className="icon-btn" onClick={props.onClose}>
            ×
          </button>
        </div>

        <section>
          <div className="row">
            <span className="k">Account</span>
            <span className="v">{s?.logged_in ? `Signed in (${s.token_source})` : 'Not signed in'}</span>
          </div>
          <div className="row">
            <span className="k">Endpoint</span>
            <span className="v">{s?.endpoint ?? '—'}</span>
          </div>
          <div className="settings-actions">
            {s?.logged_in ? (
              <button className="ghost" onClick={doLogout} disabled={busy}>
                Log out
              </button>
            ) : (
              <button className="primary" onClick={doLogin} disabled={busy}>
                {busy ? 'Waiting for browser…' : 'Log in'}
              </button>
            )}
          </div>
        </section>

        <section>
          <div className="settings-subhead">Bring your own key</div>
          <label className="field-label">Provider API key</label>
          <div className="field-row">
            <input
              className="cmd"
              type="password"
              placeholder="sk-ant-… or sk-…"
              value={key}
              onChange={(e) => setKey(e.target.value)}
            />
            <button className="ghost" onClick={saveKey} disabled={busy || !key.trim()}>
              Save
            </button>
          </div>
          <label className="field-label">Custom endpoint</label>
          <div className="field-row">
            <input
              className="cmd"
              placeholder="http://localhost:11434/v1"
              value={endpoint}
              onChange={(e) => setEndpoint(e.target.value)}
            />
            <button className="ghost" onClick={saveEndpoint} disabled={busy || !endpoint.trim()}>
              Save
            </button>
          </div>
          {saved && <div className="saved-note">{saved}</div>}
          <p className="hint">
            Stored locally by the CLI in ~/.config/memcode/.env. Keys go to the provider only. Subscription import
            (Claude/ChatGPT/Copilot) is offered in Setup (Account → Run Setup).
          </p>
        </section>

        <section className="about">
          <div className="settings-subhead">About</div>
          <div className="row">
            <span className="k">Desktop</span>
            <span className="v">{props.appInfo?.appVersion ?? '—'}</span>
          </div>
          <div className="row">
            <span className="k">Core</span>
            <span className="v">
              {s ? `${s.version} (${s.commit})` : '—'}
            </span>
          </div>
          <div className="row">
            <span className="k">Protocol</span>
            <span className="v">v{s?.protocol_version ?? '—'}</span>
          </div>
          <div className="row">
            <span className="k">Platform</span>
            <span className="v">
              {props.appInfo?.platform} · Electron {props.appInfo?.electron}
            </span>
          </div>
        </section>
      </div>
    </Modal>
  )
}

function StatusBar(props: {
  status: StatusJSON | null
  tokens: number
  mode: string
  busy: boolean
  sessionId: string | null
}) {
  const byok = props.status?.token_source && props.status.token_source !== 'environment' && props.status.token_source !== 'none'
  return (
    <footer className="statusbar">
      <span className={`dot ${props.busy ? 'busy' : 'idle'}`} />
      <span>{props.busy ? 'working' : 'ready'}</span>
      <span className="sep">·</span>
      <span>{props.mode}</span>
      {byok && (
        <>
          <span className="sep">·</span>
          <span className="byok">byok</span>
        </>
      )}
      <div className="spacer" />
      {props.sessionId && <span className="sid">{props.sessionId}</span>}
      <span className="sep">·</span>
      <span>{props.tokens.toLocaleString()} tok</span>
    </footer>
  )
}

function Modal({ children }: { children: React.ReactNode }) {
  return <div className="overlay">{children}</div>
}
