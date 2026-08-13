import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useSession, type Block } from './useSession'
import { SetupWizard } from './SetupWizard'
import { SessionSidebar } from './SessionSidebar'
import { MatrixWordmark } from './MatrixWordmark'
import { toggleTheme } from './theme'
import type { CatalogModel, SourcesJSON, StatusJSON } from '../../shared/cli-types'
import type { Attachment, DiffData, TodoItem } from '../../shared/protocol'
import type { AppInfo, MenuAction, RepoInfo } from '../../shared/ipc'

export default function App() {
  const { state, clearPermission, clearAsk, recordUserTurn, reset } = useSession()
  const [repo, setRepo] = useState<string | null>(null)
  const [sessionLive, setSessionLive] = useState(false) // a CLI session is spawned for `repo`
  const [models, setModels] = useState<CatalogModel[]>([])
  const [pin, setPin] = useState('') // '' = Automatic
  const [status, setStatus] = useState<StatusJSON | null>(null)
  const [sources, setSources] = useState<SourcesJSON | null>(null)
  const [appInfo, setAppInfo] = useState<AppInfo | null>(null)
  const [repoInfo, setRepoInfo] = useState<RepoInfo | null>(null)
  const [showSettings, setShowSettings] = useState(false)
  const [showWizard, setShowWizard] = useState(false)
  const [selectedDiff, setSelectedDiff] = useState<number | null>(null)
  const [sidebarKey, setSidebarKey] = useState(0)
  const [recents, setRecents] = useState<string[]>([])
  const bootedRef = useRef(false)

  const refreshStatus = useCallback(() => window.memcode.status().then(setStatus).catch(() => {}), [])
  const refreshSources = useCallback(() => window.memcode.sources().then(setSources).catch(() => {}), [])
  const refreshRepoInfo = useCallback((dir: string | null) => {
    if (!dir) {
      setRepoInfo(null)
      return
    }
    window.memcode.repoInfo(dir).then(setRepoInfo).catch(() => {})
  }, [])
  const refreshConfig = useCallback(() => {
    refreshStatus()
    refreshSources()
  }, [refreshStatus, refreshSources])

  useEffect(() => {
    window.memcode.models(true).then(setModels).catch(() => {})
    window.memcode.appInfo().then(setAppInfo).catch(() => {})
    refreshConfig()
    window.memcode
      .sources()
      .then((s) => {
        setSources(s)
        setShowWizard(!s.has_backend)
      })
      .catch(() => {})
  }, [refreshConfig])

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
      refreshRepoInfo(dir)
      setSessionLive(false)
      reset()
      setSidebarKey((k) => k + 1)
    },
    [refreshRepoInfo, reset],
  )

  const openRepo = useCallback(async () => {
    const dir = await window.memcode.pickRepo()
    if (!dir) return
    selectRepo(dir)
  }, [selectRepo])

  useEffect(() => {
    refreshRepoInfo(repo)
  }, [repo, state.busy, refreshRepoInfo])

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

  // Changing the model picker applies immediately to a live session (and is used
  // for the next session otherwise).
  const changePin = useCallback(
    (v: string) => {
      setPin(v)
      if (sessionLive) window.memcode.setModel(v)
    },
    [sessionLive],
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
          setSelectedDiff(null)
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

  const diffs = useMemo(() => state.blocks.filter((b): b is Extract<Block, { kind: 'diff' }> => b.kind === 'diff').map((b) => b.data), [state.blocks])
  const reviewStats = useMemo(() => summarizeDiffs(diffs), [diffs])
  const threadTitle = useMemo(() => titleFromBlocks(state.blocks, repo), [state.blocks, repo])
  useEffect(() => {
    if (selectedDiff !== null && selectedDiff >= diffs.length) setSelectedDiff(null)
  }, [diffs.length, selectedDiff])

  return (
    <div className="app">
      <div className="body">
        <SessionSidebar
          projects={repo ? [repo, ...recents.filter((r) => r !== repo)] : recents}
          activeRepo={repo}
          activeId={state.sessionId}
          settingsOpen={showSettings}
          reloadKey={sidebarKey}
          onNew={() => {
            setShowSettings(false)
            newSession()
          }}
          onSettings={() => {
            setSelectedDiff(null)
            setShowSettings(true)
          }}
          onResume={(dir, id) => {
            setShowSettings(false)
            resumeIn(dir, id)
          }}
          onSelectProject={(dir) => {
            setShowSettings(false)
            selectRepo(dir)
          }}
        />
        <div className="content">
          {showSettings ? (
            <Settings
              status={status}
              sources={sources}
              appInfo={appInfo}
              onClose={() => setShowSettings(false)}
              onChanged={refreshConfig}
              onLogin={doLogin}
              onLogout={doLogout}
            />
          ) : (
            <>
              <TopBar
                repo={repo}
                title={threadTitle}
                mode={state.mode}
                busy={state.busy}
                loggedIn={status?.logged_in ?? false}
                reviewStats={reviewStats}
                models={models}
                pin={pin}
                onPin={changePin}
                onOpenChanges={() => {
                  if (diffs.length > 0) setSelectedDiff(0)
                }}
              />
              <main className="main">
                {state.blocks.length === 0 ? (
                  <EmptyState repo={repo} recents={recents} onOpenRecent={selectRepo} loggedIn={status?.logged_in ?? false} />
                ) : (
                  <Transcript blocks={state.blocks} onOpenDiff={(index) => setSelectedDiff(index)} />
                )}
              </main>
              <LiveSessionStrip
                busy={state.busy}
                inputTokens={state.inputTokens}
                outputTokens={state.tokens}
                reasoningEffort={state.reasoningEffort}
                servedBy={state.servedBy}
                todos={state.todos}
              />
              <Composer
                noRepo={!repo}
                disabled={state.exited}
                busy={state.busy}
                onSend={send}
                onCancel={() => window.memcode.cancel()}
              />
              {selectedDiff !== null && diffs[selectedDiff] && (
                <DiffSidecar
                  diffs={diffs}
                  selected={selectedDiff}
                  onSelect={setSelectedDiff}
                  onClose={() => setSelectedDiff(null)}
                />
              )}
            </>
          )}
        </div>
      </div>

      {!showSettings && (
        <StatusBar
          status={status}
          tokens={state.tokens}
          inputTokens={state.inputTokens}
          totalOutputTokens={state.totalOutputTokens}
          cacheReadTokens={state.cacheReadTokens}
          cacheWriteTokens={state.cacheWriteTokens}
          contextTokens={state.contextTokens}
          contextWindow={state.contextWindow}
          model={state.model}
          mode={state.mode}
          busy={state.busy}
          sessionId={state.sessionId}
          servedByok={state.servedByok}
          runningShells={state.runningShells}
          repoInfo={repoInfo}
        />
      )}

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

function TopBar(props: {
  repo: string | null
  title: string
  mode: string
  busy: boolean
  loggedIn: boolean
  reviewStats: DiffSummary
  models: CatalogModel[]
  pin: string
  onPin: (v: string) => void
  onOpenChanges: () => void
}) {
  const repoName = props.repo?.split('/').pop() ?? 'No folder open'
  const hasDiffs = props.reviewStats.files > 0
  const modelGroups = useMemo(() => groupModels(props.models), [props.models])

  return (
    <header className="topbar">
      <div className="topbar-drag">
        <div className="thread-head">
          <div className="thread-title-row">
            <span className={`crumb-dot ${props.busy ? 'busy' : props.loggedIn ? 'ok' : ''}`} />
            <span className="thread-title">{props.title}</span>
            <span className="thread-more">...</span>
          </div>
          <div className="thread-meta">
            <span className="crumb-repo" title={props.repo ?? undefined}>
              {repoName}
            </span>
            <span className="crumb-sep">/</span>
            <span>{props.mode}</span>
            {hasDiffs && (
              <>
                <span className="crumb-sep">/</span>
                <span className="diff-count">
                  {props.reviewStats.files} files <span className="add">+{props.reviewStats.added}</span>{' '}
                  <span className="del">-{props.reviewStats.removed}</span>
                </span>
              </>
            )}
          </div>
        </div>
      </div>
      <div className="topbar-actions">
        <label className="model-selector topbar-model-selector" title="Model">
          <span className="model-dot" />
          <select value={props.pin} onChange={(e) => props.onPin(e.target.value)} aria-label="Model">
            <option value="">Automatic</option>
            {modelGroups.map((group) => (
              <optgroup key={group.name} label={group.name}>
                {group.models.map((m) => (
                  <option key={m.id} value={m.id}>
                    {modelOptionLabel(m)}
                  </option>
                ))}
              </optgroup>
            ))}
          </select>
        </label>
        {hasDiffs && (
          <button className="toolbar-btn" onClick={props.onOpenChanges} title="Open changed files">
            Changes
          </button>
        )}
      </div>
    </header>
  )
}

interface DiffSummary {
  files: number
  added: number
  removed: number
}

function summarizeDiffs(diffs: DiffData[]): DiffSummary {
  return diffs.reduce(
    (acc, d) => ({
      files: acc.files + 1,
      added: acc.added + d.added,
      removed: acc.removed + d.removed,
    }),
    { files: 0, added: 0, removed: 0 },
  )
}

function titleFromBlocks(blocks: Block[], repo: string | null): string {
  for (let i = blocks.length - 1; i >= 0; i--) {
    const b = blocks[i]
    if (b.kind === 'user' && b.text.trim()) return truncateTitle(b.text)
  }
  return repo ? `Work in ${repo.split('/').pop()}` : 'New task'
}

function truncateTitle(text: string): string {
  const first = text.trim().split('\n')[0].replace(/\s+/g, ' ')
  return first.length > 56 ? `${first.slice(0, 53)}...` : first
}

function EmptyState(props: {
  repo: string | null
  recents: string[]
  onOpenRecent: (dir: string) => void
  loggedIn: boolean
}) {
  return (
    <div className={`empty ${props.repo ? 'repo-ready' : ''}`}>
      <MatrixWordmark className="empty-wordmark" />
      {!props.repo && (
        <>
          <p className="empty-tag">
            Pick a folder from File &gt; Open Folder, or press <kbd>⌘O</kbd>.
          </p>
          {props.recents.length > 0 && (
            <div className="recents">
              <div className="recents-title">Recent</div>
              {props.recents.map((r) => (
                <button key={r} className="recent-item" title={r} onClick={() => props.onOpenRecent(r)}>
                  <span className="recent-icon" aria-hidden="true">
                    /
                  </span>
                  <span className="recent-text">
                    <span className="recent-name">{r.split('/').pop()}</span>
                    <span className="recent-path">{r}</span>
                  </span>
                </button>
              ))}
            </div>
          )}
          {!props.loggedIn && <p className="hint">Not signed in. Open Settings to log in or set a BYOK key.</p>}
        </>
      )}
    </div>
  )
}

function Transcript({ blocks, onOpenDiff }: { blocks: Block[]; onOpenDiff: (index: number) => void }) {
  const endRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    endRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [blocks])
  let diffIndex = -1
  return (
    <div className="transcript">
      {blocks.map((b, i) => {
        if (b.kind === 'diff') diffIndex += 1
        return <BlockView key={i} block={b} diffIndex={b.kind === 'diff' ? diffIndex : undefined} onOpenDiff={onOpenDiff} />
      })}
      <div ref={endRef} />
    </div>
  )
}

function BlockView({
  block,
  diffIndex,
  onOpenDiff,
}: {
  block: Block
  diffIndex?: number
  onOpenDiff: (index: number) => void
}) {
  switch (block.kind) {
    case 'user':
      return (
        <div className="msg user">
          <div className="msg-text">{block.text}</div>
          {block.attachments.length > 0 && (
            <div className="chips">
              {block.attachments.map((a, i) => (
                <span className="chip" key={i}>
                  <span className="chip-icon">+</span> {a}
                </span>
              ))}
            </div>
          )}
        </div>
      )
    case 'assistant':
      return (
        <div className="msg assistant">
          <MarkdownContent>{block.text}</MarkdownContent>
        </div>
      )
    case 'tool':
      return (
        <div className={`tool-block tool-${block.status}`}>
          <div className="tool">
            <span className="tool-glyph">{block.status === 'failed' ? '✗' : '●'}</span>
            <span className="tool-name">{toolLabel(block.name)}</span>
            {block.target && (
              <>
                <span className="tool-paren">(</span>
                <span className="tool-target">{block.target}</span>
                <span className="tool-paren">)</span>
              </>
            )}
            {block.detail && <span className="tool-detail">· {block.detail}</span>}
            {block.status === 'running' && <span className="tool-status">running</span>}
          </div>
          {block.output && <div className="tool-output">{block.output}</div>}
        </div>
      )
    case 'diff':
      return <DiffView data={block.data} onOpen={() => onOpenDiff(diffIndex ?? 0)} />
    case 'error':
      return <div className="msg error">{block.message}</div>
  }
}

function MarkdownContent({ children }: { children: string }) {
  const clean = typeof children === 'string' ? children.replace(/<thinking>[\s\S]*?<\/thinking>/g, '') : ''
  return (
    <div className="markdown-content">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          a: ({ children: linkChildren, ...props }) => (
            <a {...props} target="_blank" rel="noreferrer">
              {linkChildren}
            </a>
          ),
        }}
      >
        {clean}
      </ReactMarkdown>
    </div>
  )
}

function toolLabel(name: string): string {
  const normalized = name.toLowerCase()
  if (/(edit|write|patch|update|replace|create)/.test(normalized)) return 'Update'
  if (/(read|cat|open)/.test(normalized)) return 'Read'
  if (/(grep|search|rg|find)/.test(normalized)) return 'Search'
  if (/(list|ls|glob)/.test(normalized)) return 'List'
  if (/(bash|shell|exec|command|run)/.test(normalized)) return 'Bash'
  if (/(test|check|diagnostic|lint|typecheck|build)/.test(normalized)) return 'Check'

  return name
    .split(/[_\s-]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join('')
}

function DiffView({ data, onOpen }: { data: DiffData; onOpen: () => void }) {
  return (
    <div className="diff">
      <button className="diff-head" onClick={onOpen}>
        <span className="diff-glyph">●</span>
        <span className="diff-action">{data.new_file ? 'Create' : 'Update'}</span>
        <span className="diff-paren">(</span>
        <span className="diff-path" title={data.path}>
          {data.path}
        </span>
        <span className="diff-paren">)</span>
        <span className="diff-stat">
          <span className="add">+{data.added}</span> <span className="del">−{data.removed}</span>
        </span>
        <span className="diff-open">Open diff</span>
      </button>
    </div>
  )
}

function LiveSessionStrip({
  busy,
  inputTokens,
  outputTokens,
  reasoningEffort,
  servedBy,
  todos,
}: {
  busy: boolean
  inputTokens: number
  outputTokens: number
  reasoningEffort: string
  servedBy: string
  todos: TodoItem[]
}) {
  const [tick, setTick] = useState(0)
  const busyStartRef = useRef<number | null>(null)
  const inputStartRef = useRef(inputTokens)

  useEffect(() => {
    if (!busy) {
      busyStartRef.current = null
      inputStartRef.current = inputTokens
      setTick(0)
      return
    }
    if (busyStartRef.current === null) {
      busyStartRef.current = Date.now()
      inputStartRef.current = inputTokens
    }
    const id = window.setInterval(() => setTick((n) => n + 1), 1000)
    return () => window.clearInterval(id)
  }, [busy, inputTokens])

  if (!busy && todos.length === 0) return null
  const done = todos.filter((t) => t.status === 'done').length
  const elapsed = busyStartRef.current === null ? 0 : Math.floor((Date.now() - busyStartRef.current) / 1000)
  const inputDelta = Math.max(0, inputTokens - inputStartRef.current)
  const meta = [`${formatDuration(elapsed)}`]
  if (inputDelta > 0 || outputTokens > 0) meta.push(`↑${formatCompact(inputDelta)} ↓${formatCompact(outputTokens)}`)
  if (servedBy) meta.push(`served by ${servedBy}`)
  if (reasoningEffort) meta.push(`${reasoningEffort} effort`)
  meta.push('esc to interrupt')

  return (
    <section className="live-strip" aria-label="Live session status">
      {busy && (
        <div className="live-status">
          <span className="live-spinner">{spinFrame(tick)}</span>
          <span className="live-thinking"> Thinking… </span>
          <span className="live-muted" title={meta.join(' · ')}>
            ({meta.join(' · ')})
          </span>
        </div>
      )}
      {todos.length > 0 && (
        <div className="live-todos">
          <div className="live-todos-title">
            Tasks {done}/{todos.length}
          </div>
          {todos.map((todo, index) => (
            <div className={`live-todo s-${todo.status}`} key={`${todo.status}-${index}-${todo.text}`}>
              <span className="live-todo-glyph">{todoGlyph(todo.status)}</span>
              <span>{todo.text}</span>
            </div>
          ))}
        </div>
      )}
    </section>
  )
}

function todoGlyph(status: string): string {
  if (status === 'done') return '√'
  if (status === 'in_progress') return '▸'
  if (status === 'blocked') return '⊘'
  if (status === 'skipped') return '⊝'
  return '○'
}

function spinFrame(tick: number): string {
  return '⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏'[tick % 10] ?? '⠋'
}

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const rest = seconds % 60
  if (minutes < 60) return rest === 0 ? `${minutes}m` : `${minutes}m${rest}s`
  const hours = Math.floor(minutes / 60)
  const mins = minutes % 60
  return mins === 0 ? `${hours}h` : `${hours}h${mins}m`
}

function formatCompact(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`
  return String(n)
}

function DiffPanel({
  diffs,
  selected,
  onSelect,
}: {
  diffs: DiffData[]
  selected: number
  onSelect: (index: number) => void
}) {
  const current = diffs[Math.min(selected, diffs.length - 1)]
  const stats = summarizeDiffs(diffs)

  if (!current) return null

  return (
    <section className="review-panel">
      <div className="review-head">
        <div>
          <div className="review-title">Diff</div>
          <div className="review-summary">
            {stats.files} files <span className="add">+{stats.added}</span> <span className="del">-{stats.removed}</span>
          </div>
        </div>
      </div>
      <div className="review-files">
        {diffs.map((d, i) => (
          <button key={`${d.path}-${i}`} className={`review-file ${i === selected ? 'active' : ''}`} onClick={() => onSelect(i)}>
            <span className="review-file-icon">{d.new_file ? '+' : 'M'}</span>
            <span className="review-file-name" title={d.path}>
              {d.path}
            </span>
            <span className="review-file-stat">
              <span className="add">+{d.added}</span> <span className="del">-{d.removed}</span>
            </span>
          </button>
        ))}
      </div>
      <div className="review-diff">
        <div className="review-diff-head">
          <span className="review-file-name" title={current.path}>
            {current.path}
          </span>
          <span className="review-file-stat">
            <span className="add">+{current.added}</span> <span className="del">-{current.removed}</span>
          </span>
        </div>
        <pre className="review-diff-body">
          {(current.unified || '').split('\n').map((line, i) => (
            <div key={i} className={diffLineClass(line)}>
              {line || ' '}
            </div>
          ))}
        </pre>
      </div>
    </section>
  )
}

function DiffSidecar(props: {
  diffs: DiffData[]
  selected: number
  onSelect: (index: number) => void
  onClose: () => void
}) {
  return (
    <aside className="sidecar sidecar-diff">
      <button className="sidecar-close" onClick={props.onClose} title="Close">
        ×
      </button>
      <DiffPanel diffs={props.diffs} selected={props.selected} onSelect={props.onSelect} />
    </aside>
  )
}

function diffLineClass(line: string): string {
  if (line.startsWith('+') && !line.startsWith('+++')) return 'dl add'
  if (line.startsWith('-') && !line.startsWith('---')) return 'dl del'
  if (line.startsWith('@@')) return 'dl hunk'
  return 'dl'
}

function Composer(props: {
  noRepo: boolean
  disabled: boolean
  busy: boolean
  onSend: (text: string, attachments: Attachment[]) => void
  onCancel: () => void
}) {
  const [text, setText] = useState('')
  const [atts, setAtts] = useState<Attachment[]>([])
  const [attaching, setAttaching] = useState(false)
  const [dragging, setDragging] = useState(false)
  const [slashSel, setSlashSel] = useState(0)
  const taRef = useRef<HTMLTextAreaElement>(null)
  const slashItems = useMemo(() => matchSlashCommands(text), [text])
  const slashOpen = slashItems.length > 0

  const grow = () => {
    const ta = taRef.current
    if (!ta) return
    ta.style.height = 'auto'
    ta.style.height = `${Math.min(ta.scrollHeight, 240)}px`
  }

  const submit = () => {
    if (!text.trim() && atts.length === 0) return
    if (attaching) return
    props.onSend(text, atts)
    setText('')
    setAtts([])
    requestAnimationFrame(grow)
  }

  const addFiles = async (files: Iterable<File>) => {
    setAttaching(true)
    const next: Attachment[] = []
    try {
      for (const f of Array.from(files)) {
        const att = await fileToAttachment(f)
        if (att) next.push(att)
      }
      if (next.length) setAtts((prev) => [...prev, ...next])
    } finally {
      setAttaching(false)
    }
  }

  const addLargePaste = async (pastedText: string) => {
    setAttaching(true)
    try {
      const bytes = new TextEncoder().encode(pastedText).buffer
      const att = await window.memcode.saveAttachment({ name: 'Pasted text.txt', mime: 'text/plain', bytes })
      setAtts((prev) => [...prev, att])
    } finally {
      setAttaching(false)
    }
  }

  const onDrop = (e: React.DragEvent) => {
    e.preventDefault()
    setDragging(false)
    void addFiles(e.dataTransfer.files)
  }

  const completeSlash = (index = slashSel) => {
    const item = slashItems[Math.min(index, slashItems.length - 1)]
    if (!item) return
    setText(`${item.name} `)
    setSlashSel(0)
    requestAnimationFrame(() => {
      grow()
      taRef.current?.focus()
    })
  }

  useEffect(() => {
    setSlashSel(0)
  }, [slashItems.length])

  return (
    <div className="composer-wrap">
      {slashOpen && (
        <div className="slash-menu" role="listbox" aria-label="Memcode slash commands">
          <div className="slash-menu-title">Memcode commands</div>
          <div className="slash-list">
            {slashItems.map((item, index) => (
              <button
                key={item.name}
                type="button"
                className={`slash-item ${index === slashSel ? 'active' : ''}`}
                onMouseDown={(e) => {
                  e.preventDefault()
                  completeSlash(index)
                }}
              >
                <span className="slash-icon" aria-hidden="true">
                  {item.icon}
                </span>
                <span className="slash-name">{item.name.slice(1)}</span>
                <span className="slash-desc">{item.desc}</span>
              </button>
            ))}
          </div>
        </div>
      )}
      <form
        className={`composer ${dragging ? 'dragging' : ''} ${props.noRepo ? 'disabled' : ''}`}
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
        onDragOver={(e) => {
          if (props.noRepo) return
          e.preventDefault()
          setDragging(true)
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={onDrop}
      >
        {atts.length > 0 && (
          <div className="composer-attachments">
            {atts.map((a, i) => (
              <span className="chip" key={i}>
                <span className="chip-icon" aria-hidden="true" />
                <span className="chip-label" title={a.path}>
                  {a.name}
                </span>
                <button type="button" className="chip-x" onClick={() => setAtts(atts.filter((_, j) => j !== i))} title="Remove attachment">
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
              ? 'Open a folder to start a session...'
              : dragging
                ? 'Drop files to attach...'
                : 'Ask anything...'
          }
          value={text}
          disabled={props.disabled || props.noRepo}
          rows={1}
          onChange={(e) => {
            setText(e.target.value)
            grow()
          }}
          onKeyDown={(e) => {
            if (slashOpen) {
              if (e.key === 'ArrowDown') {
                e.preventDefault()
                setSlashSel((n) => Math.min(n + 1, slashItems.length - 1))
                return
              }
              if (e.key === 'ArrowUp') {
                e.preventDefault()
                setSlashSel((n) => Math.max(n - 1, 0))
                return
              }
              if (e.key === 'Tab') {
                e.preventDefault()
                completeSlash()
                return
              }
              if (e.key === 'Enter') {
                e.preventDefault()
                completeSlash()
                return
              }
            }
            if (e.key === 'Enter' && !e.shiftKey) {
              e.preventDefault()
              submit()
            }
          }}
          onPaste={(e) => {
            const pastedFiles = filesFromClipboard(e.clipboardData)
            if (pastedFiles.length > 0) {
              e.preventDefault()
              void addFiles(pastedFiles)
              return
            }
            const pastedText = e.clipboardData.getData('text/plain')
            if (pastedText.length > 2000) {
              e.preventDefault()
              void addLargePaste(pastedText)
            }
          }}
        />
        {attaching && (
          <div className="composer-attachments">
            <span className="chip pending">Processing attachment...</span>
          </div>
        )}
        <div className="composer-toolbar">
          <div className="composer-toolbar-left">
            <span className="composer-mode-chip" title="Current session mode">
              {props.noRepo ? 'No folder' : 'Ask'}
            </span>
          </div>
          <div className="composer-toolbar-right">
            {props.busy ? (
              <button type="button" className="stop-btn" onClick={props.onCancel} title="Stop">
                ■
              </button>
            ) : (
              <button type="submit" className="send-btn" disabled={props.noRepo || props.disabled || attaching || (!text.trim() && atts.length === 0)} title="Send">
                ↑
              </button>
            )}
          </div>
        </div>
      </form>
    </div>
  )
}

async function fileToAttachment(file: File): Promise<Attachment | null> {
  const path = (file as File & { path?: string }).path
  const name = file.name || defaultAttachmentName(file.type)
  const mime = file.type || undefined
  if (path) return { path, name, mime }
  const bytes = await file.arrayBuffer()
  return window.memcode.saveAttachment({ name, mime, bytes })
}

function filesFromClipboard(data: DataTransfer): File[] {
  const files: File[] = []
  for (const item of Array.from(data.items)) {
    if (item.kind !== 'file') continue
    const file = item.getAsFile()
    if (file) files.push(file)
  }
  return files
}

function defaultAttachmentName(mime: string): string {
  if (mime === 'image/png') return 'Pasted image.png'
  if (mime === 'image/jpeg') return 'Pasted image.jpg'
  if (mime === 'image/gif') return 'Pasted image.gif'
  if (mime === 'image/webp') return 'Pasted image.webp'
  if (mime.startsWith('image/')) return `Pasted image.${mime.slice('image/'.length) || 'png'}`
  return 'Pasted file'
}

interface SlashCommand {
  name: string
  desc: string
  icon: string
}

const slashCommands: SlashCommand[] = [
  { name: '/help', desc: 'show commands', icon: '?' },
  { name: '/login', desc: 'sign in to memcode.ai', icon: 'in' },
  { name: '/logout', desc: 'sign out of this machine', icon: 'out' },
  { name: '/advisor', desc: 'outside second opinion', icon: 'ad' },
  { name: '/next', desc: 'highest-value next move', icon: 'nx' },
  { name: '/recap', desc: 'recap this session', icon: 'rc' },
  { name: '/overview', desc: 'what this project is', icon: 'ov' },
  { name: '/arch', desc: 'architecture / flow diagrams', icon: 'ar' },
  { name: '/doctor', desc: 'runtime health check', icon: 'dr' },
  { name: '/jobs', desc: 'background jobs', icon: 'jb' },
  { name: '/tail', desc: 'tail a background job', icon: 'tl' },
  { name: '/kill', desc: 'kill a background job', icon: 'kl' },
  { name: '/status', desc: 'session status line', icon: 'st' },
  { name: '/plan', desc: 'plan mode', icon: 'pl' },
  { name: '/yolo', desc: 'plan + auto-execute', icon: 'yo' },
  { name: '/dispatch', desc: 'dispatch a hands-off sub-agent', icon: 'ds' },
  { name: '/agents', desc: 'list dispatched sub-agents', icon: 'ag' },
  { name: '/sync', desc: 'sync memory to assistants', icon: 'sy' },
  { name: '/mode', desc: 'change permission mode', icon: 'md' },
  { name: '/theme', desc: 'change display theme', icon: 'th' },
  { name: '/personality', desc: "set the agent's voice", icon: 'ps' },
  { name: '/extramile', desc: 'edge cases + completeness', icon: 'ex' },
  { name: '/effort', desc: 'force thinking effort', icon: 'ef' },
  { name: '/goal', desc: 'set an objective', icon: 'go' },
  { name: '/model', desc: 'pick or pin a model', icon: 'mo' },
  { name: '/apikeys', desc: 'bring your own provider API keys', icon: 'ky' },
  { name: '/websites', desc: 'your AI-built websites', icon: 'ws' },
  { name: '/cost', desc: 'session spend', icon: '$' },
  { name: '/costp', desc: 'spend by purpose', icon: '$p' },
  { name: '/costby', desc: 'spend by backend', icon: '$b' },
  { name: '/todos', desc: 'show active task checklist', icon: 'td' },
  { name: '/debug', desc: 'runtime debug summary', icon: 'db' },
  { name: '/compact', desc: 'summarize older turns', icon: 'cp' },
  { name: '/clear', desc: 'start a fresh session', icon: 'cl' },
  { name: '/resume', desc: 're-enter a previous session', icon: 'rs' },
  { name: '/fork', desc: 'fork this conversation', icon: 'fk' },
  { name: '/rewind', desc: 'undo agent edits', icon: 'rw' },
  { name: '/quit', desc: 'exit', icon: 'qt' },
]

function matchSlashCommands(text: string): SlashCommand[] {
  const trimmed = text.trimStart()
  if (!trimmed.startsWith('/') || /\s/.test(trimmed)) return []
  const query = trimmed.toLowerCase()
  return slashCommands.filter((command) => command.name.startsWith(query))
}

function groupModels(models: CatalogModel[]): Array<{ name: string; models: CatalogModel[] }> {
  const groups = new Map<string, CatalogModel[]>()
  for (const model of models) {
    const name = providerName(model.vendor || model.group || 'Models')
    const list = groups.get(name) ?? []
    list.push(model)
    groups.set(name, list)
  }
  return Array.from(groups, ([name, list]) => ({
    name,
    models: list.slice().sort((a, b) => (a.name || a.label || a.id).localeCompare(b.name || b.label || b.id)),
  })).sort((a, b) => providerRank(a.name) - providerRank(b.name) || a.name.localeCompare(b.name))
}

function providerName(vendor: string): string {
  const v = vendor.toLowerCase()
  if (v.includes('openai')) return 'OpenAI'
  if (v.includes('anthropic') || v.includes('claude')) return 'Anthropic'
  if (v.includes('google') || v.includes('gemini')) return 'Google'
  if (v.includes('xai') || v.includes('grok')) return 'xAI'
  if (v.includes('fireworks')) return 'Fireworks'
  if (v.includes('memcode')) return 'Memcode'
  return vendor || 'Models'
}

function providerRank(name: string): number {
  return ['OpenAI', 'Anthropic', 'Google', 'xAI', 'Fireworks', 'Memcode'].indexOf(name) + 1 || 99
}

function modelOptionLabel(model: CatalogModel): string {
  const label = model.name || model.label || model.id
  const ctx = model.window ? ` ${formatContext(model.window)}` : ''
  return `${label}${ctx}`
}

function formatContext(window: number): string {
  if (window >= 1_000_000) return `(${Math.round(window / 1_000_000)}M)`
  if (window >= 1_000) return `(${Math.round(window / 1_000)}K)`
  return `(${window})`
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

const BYOK_PROVIDERS = [
  { id: 'anthropic', label: 'Anthropic', env: 'ANTHROPIC_API_KEY', endpoint: 'https://api.anthropic.com', direct: true },
  { id: 'openai', label: 'OpenAI', env: 'OPENAI_API_KEY', endpoint: 'https://api.openai.com/v1', direct: true },
  { id: 'gemini', label: 'Google Gemini', env: 'GEMINI_API_KEY', endpoint: 'https://generativelanguage.googleapis.com', direct: false },
  { id: 'xai', label: 'xAI', env: 'XAI_API_KEY', endpoint: 'https://api.x.ai/v1', direct: false },
  { id: 'fireworks', label: 'Fireworks', env: 'FIREWORKS_API_KEY', endpoint: 'https://api.fireworks.ai/inference/v1', direct: false },
  { id: 'groq', label: 'Groq', env: 'GROQ_API_KEY', endpoint: 'https://api.groq.com/openai/v1', direct: false },
  { id: 'mistral', label: 'Mistral', env: 'MISTRAL_API_KEY', endpoint: 'https://api.mistral.ai/v1', direct: false },
  { id: 'deepseek', label: 'DeepSeek', env: 'DEEPSEEK_API_KEY', endpoint: 'https://api.deepseek.com/v1', direct: false },
  { id: 'together', label: 'Together', env: 'TOGETHER_API_KEY', endpoint: 'https://api.together.xyz/v1', direct: false },
  { id: 'openrouter', label: 'OpenRouter', env: 'OPENROUTER_API_KEY', endpoint: 'https://openrouter.ai/api/v1', direct: false },
  { id: 'cerebras', label: 'Cerebras', env: 'CEREBRAS_API_KEY', endpoint: 'https://api.cerebras.ai/v1', direct: false },
] as const

const ENDPOINT_PRESETS = [
  { label: 'Ollama local', value: 'http://localhost:11434/v1' },
  { label: 'LM Studio local', value: 'http://localhost:1234/v1' },
  ...BYOK_PROVIDERS.map((p) => ({ label: p.label, value: p.endpoint })),
]

type SettingsSection = 'general' | 'account' | 'providers' | 'subscriptions' | 'endpoint' | 'about'

const SETTINGS_SECTIONS: Array<{ id: SettingsSection; label: string; group: 'Personal' | 'Coding' | 'System' }> = [
  { id: 'general', label: 'General', group: 'Personal' },
  { id: 'account', label: 'Account', group: 'Personal' },
  { id: 'providers', label: 'BYOK providers', group: 'Coding' },
  { id: 'subscriptions', label: 'Subscriptions', group: 'Coding' },
  { id: 'endpoint', label: 'Custom endpoint', group: 'Coding' },
  { id: 'about', label: 'About', group: 'System' },
]

function Settings(props: {
  status: StatusJSON | null
  sources: SourcesJSON | null
  appInfo: AppInfo | null
  onClose: () => void
  onChanged: () => void
  onLogin: () => Promise<void>
  onLogout: () => Promise<void>
}) {
  const [busy, setBusy] = useState(false)
  const [section, setSection] = useState<SettingsSection>('general')
  const [search, setSearch] = useState('')
  const [providerId, setProviderId] = useState<(typeof BYOK_PROVIDERS)[number]['id']>('anthropic')
  const [providerKey, setProviderKey] = useState('')
  const [routeProvider, setRouteProvider] = useState(false)
  const [endpoint, setEndpoint] = useState('')
  const [saved, setSaved] = useState('')
  const [err, setErr] = useState('')

  const withBusy = async (fn: () => Promise<unknown>) => {
    setBusy(true)
    setSaved('')
    setErr('')
    try {
      await fn()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }
  const doLogin = () => withBusy(props.onLogin)
  const doLogout = () => withBusy(props.onLogout)
  const selectedProvider = BYOK_PROVIDERS.find((p) => p.id === providerId) ?? BYOK_PROVIDERS[0]
  const saveProviderKey = () =>
    withBusy(async () => {
      const v = providerKey.trim()
      if (!v) return
      const kv: Record<string, string> = { [selectedProvider.env]: v }
      if (routeProvider) kv.MEMCODE_ENDPOINT_URL = selectedProvider.endpoint
      await window.memcode.setConfig(kv)
      setProviderKey('')
      setSaved(routeProvider ? `Saved ${selectedProvider.env} and selected ${selectedProvider.label}` : `Saved ${selectedProvider.env}`)
      props.onChanged()
    })
  const useSubscription = (id: string, label: string) =>
    withBusy(async () => {
      await window.memcode.setConfig({ MEMCODE_CREDENTIAL_SOURCE: id })
      setSaved(`Selected ${label}`)
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
  const src = props.sources
  const title = SETTINGS_SECTIONS.find((item) => item.id === section)?.label ?? 'Settings'
  const query = search.trim().toLowerCase()
  const visibleSections = query ? SETTINGS_SECTIONS.filter((item) => item.label.toLowerCase().includes(query) || item.group.toLowerCase().includes(query)) : SETTINGS_SECTIONS

  return (
    <div className="settings-page">
      <aside className="settings-rail">
        <button className="settings-back" type="button" onClick={props.onClose}>
          <span>←</span> Back to app
        </button>
        <div className="settings-search">
          <span>⌕</span>
          <input placeholder="Search settings..." value={search} onChange={(e) => setSearch(e.target.value)} />
        </div>
        {(['Personal', 'Coding', 'System'] as const).map((group) => (
          <div className="settings-nav-group" key={group}>
            <div className="settings-nav-title">{group}</div>
            {visibleSections.filter((item) => item.group === group).map((item) => (
              <button
                key={item.id}
                type="button"
                className={`settings-nav-item ${section === item.id ? 'active' : ''}`}
                onClick={() => setSection(item.id)}
              >
                {item.label}
              </button>
            ))}
          </div>
        ))}
      </aside>

      <main className="settings-main">
        <div className="settings-main-inner">
          <h1>{title}</h1>

          {section === 'general' && (
            <>
              <section className="settings-card">
                <div className="settings-card-title">Permissions</div>
                <div className="settings-row">
                  <div>
                    <div className="settings-row-title">Default permissions</div>
                    <p>Memcode can read and edit files in the active project folder.</p>
                  </div>
                  <span className="switch on" />
                </div>
                <div className="settings-row">
                  <div>
                    <div className="settings-row-title">Full access badge</div>
                    <p>Show the current access mode in the composer control row.</p>
                  </div>
                  <span className="switch on" />
                </div>
              </section>
              <section className="settings-card">
                <div className="settings-card-title">Chat</div>
                <div className="settings-row">
                  <div>
                    <div className="settings-row-title">Markdown rendering</div>
                    <p>Assistant messages render GitHub-flavored markdown, including lists, code blocks, tables, and links.</p>
                  </div>
                  <span className="switch on" />
                </div>
                <div className="settings-row">
                  <div>
                    <div className="settings-row-title">Model selector</div>
                    <p>Show the model selector in the composer toolbar.</p>
                  </div>
                  <span className="switch on" />
                </div>
              </section>
            </>
          )}

          {section === 'account' && (
            <section className="settings-card">
              <div className="settings-card-title">memcode.ai</div>
              <div className="settings-row">
                <div>
                  <div className="settings-row-title">Account</div>
                  <p>{s?.logged_in ? `Signed in with ${s.token_source}` : 'Not signed in'}</p>
                </div>
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
              <div className="settings-row">
                <div>
                  <div className="settings-row-title">Active backend</div>
                  <p>{src?.credential_source || src?.endpoint || s?.endpoint || (s?.logged_in ? 'hosted' : 'none')}</p>
                </div>
              </div>
            </section>
          )}

          {section === 'subscriptions' && (
            <section className="settings-card">
              <div className="settings-card-title">Detected subscription sources</div>
              <div className="provider-grid">
                {src?.subscriptions.length ? (
                  src.subscriptions.map((sub) => (
                    <button
                      key={sub.id}
                      className={`provider-tile ${src.credential_source === sub.id ? 'active' : ''}`}
                      onClick={() => useSubscription(sub.id, sub.label)}
                      disabled={busy}
                    >
                      <span className="provider-name">{sub.label}</span>
                      <span className="provider-meta">{src.credential_source === sub.id ? 'Selected' : 'Detected'}</span>
                    </button>
                  ))
                ) : (
                  <div className="settings-note">No Claude, ChatGPT/Codex, or Copilot subscription source was detected.</div>
                )}
              </div>
            </section>
          )}

          {section === 'providers' && (
            <section className="settings-card">
              <div className="settings-card-title">Bring your own key</div>
              <div className="provider-grid compact">
                {BYOK_PROVIDERS.map((p) => (
                  <button
                    key={p.id}
                    className={`provider-tile ${providerId === p.id ? 'active' : ''}`}
                    onClick={() => {
                      setProviderId(p.id)
                      setRouteProvider(!p.direct)
                    }}
                  >
                    <span className="provider-name">{p.label}</span>
                    <span className="provider-meta">{p.direct ? 'Direct key' : 'Endpoint key'}</span>
                  </button>
                ))}
              </div>
              <label className="field-label">{selectedProvider.label} API key</label>
              <div className="field-row">
                <input
                  className="cmd"
                  type="password"
                  placeholder={selectedProvider.env}
                  value={providerKey}
                  onChange={(e) => setProviderKey(e.target.value)}
                />
                <button className="ghost" onClick={saveProviderKey} disabled={busy || !providerKey.trim()}>
                  Save
                </button>
              </div>
              <label className="remember inline">
                <input type="checkbox" checked={routeProvider} onChange={(e) => setRouteProvider(e.target.checked)} /> Use{' '}
                {selectedProvider.label} as the active endpoint
              </label>
              <p className="settings-note">
                Anthropic and OpenAI keys can run directly. Other provider keys are saved for their matching endpoint.
              </p>
            </section>
          )}

          {section === 'endpoint' && (
            <section className="settings-card">
              <div className="settings-card-title">Custom endpoint</div>
              <div className="preset-row">
                {ENDPOINT_PRESETS.map((p) => (
                  <button key={p.value} className="preset" onClick={() => setEndpoint(p.value)}>
                    {p.label}
                  </button>
                ))}
              </div>
              <label className="field-label">Endpoint URL</label>
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
              <p className="settings-note">Stored locally by the CLI in ~/.config/memcode/.env. Secrets are passed over stdin.</p>
            </section>
          )}

          {section === 'about' && (
            <section className="settings-card about">
              <div className="settings-card-title">Versions</div>
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
          )}

          {(saved || err) && (
            <div className="settings-toast">
              {saved && <span className="saved-note">{saved}</span>}
              {err && <span className="settings-err">{err}</span>}
            </div>
          )}
        </div>
      </main>
    </div>
  )
}

function StatusBar(props: {
  status: StatusJSON | null
  tokens: number
  inputTokens: number
  totalOutputTokens: number
  cacheReadTokens: number
  cacheWriteTokens: number
  contextTokens: number
  contextWindow: number
  model: string
  mode: string
  busy: boolean
  sessionId: string | null
  servedByok: boolean
  runningShells: number
  repoInfo: RepoInfo | null
}) {
  const segments: Array<{ text: string; className?: string }> = [{ text: 'memcode', className: 'status-brand' }]
  const build = buildCompact(props.status)
  if (build) segments.push({ text: build })
  if (props.repoInfo?.branch) segments.push({ text: props.repoInfo.branch })
  if (props.repoInfo) segments.push({ text: gitStatText(props.repoInfo) })
  if (props.inputTokens > 0 || props.totalOutputTokens > 0 || props.tokens > 0) {
    segments.push({ text: `↑${formatCompact(props.inputTokens)} ↓${formatCompact(props.totalOutputTokens || props.tokens)}` })
    if (props.cacheReadTokens > 0) {
      segments.push({ text: `◌ ${cacheHitRate(props.inputTokens, props.cacheReadTokens)}%` })
    }
  }
  if (props.contextTokens > 0 && props.contextWindow > 0) {
    const pct = Math.floor((props.contextTokens * 100) / props.contextWindow)
    segments.push({ text: `ctx ${pct}%`, className: pct >= 80 ? 'warn' : undefined })
  }
  if (props.model) segments.push({ text: shortModel(props.model) })
  if (props.servedByok) segments.push({ text: 'byok', className: 'byok' })
  if (props.runningShells > 0) segments.push({ text: `${props.busy ? '⠋ ' : ''}${props.runningShells} shell${props.runningShells === 1 ? '' : 's'}` })
  segments.push({ text: props.mode })

  return (
    <footer className="statusbar">
      {segments.map((segment, index) => (
        <span key={`${segment.text}-${index}`} className={segment.className}>
          {index > 0 && <span className="sep">·</span>}
          {segment.text}
        </span>
      ))}
      <div className="spacer" />
      {props.sessionId && <span className="sid">{props.sessionId}</span>}
    </footer>
  )
}

function buildCompact(status: StatusJSON | null): string {
  if (!status?.version) return ''
  return status.version
}

function gitStatText(info: RepoInfo): string {
  const clean =
    info.files === 0 &&
    info.added === 0 &&
    info.removed === 0 &&
    info.stagedFiles === 0 &&
    info.stagedAdded === 0 &&
    info.stagedRemoved === 0
  if (clean) return 'clean'
  const parts: string[] = []
  if (info.files > 0) parts.push(`git ${info.files}✎ +${formatCompact(info.added)}/-${formatCompact(info.removed)}`)
  if (info.stagedFiles > 0) {
    parts.push(`staged ${info.stagedFiles}✎ +${formatCompact(info.stagedAdded)}/-${formatCompact(info.stagedRemoved)}`)
  }
  return parts.join(' ')
}

function cacheHitRate(input: number, read: number): number {
  return input + read === 0 ? 0 : Math.floor((read * 100) / (input + read))
}

function shortModel(model: string): string {
  const tail = model.split('/').filter(Boolean).pop() ?? model
  return tail
    .replace(/^claude-/, '')
    .replace(/^gpt-/, 'gpt-')
    .replace(/-\d{8}$/, '')
}

function Modal({ children }: { children: React.ReactNode }) {
  return <div className="overlay">{children}</div>
}
