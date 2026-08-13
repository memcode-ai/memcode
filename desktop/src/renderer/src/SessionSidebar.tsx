import { useEffect, useState } from 'react'
import type { SessionRecent } from '../../shared/cli-types'

// Sidebar grouped by project (recent folders), each with its resumable chats
// nested under it — like a normal chat app. Titles come from the CLI (generated
// server-side); only resumable conversations are listed and clickable.
export function SessionSidebar(props: {
  projects: string[]
  activeRepo: string | null
  activeId: string | null
  settingsOpen: boolean
  reloadKey: number
  onNew: () => void
  onSettings: () => void
  onResume: (dir: string, id: string) => void
  onSelectProject: (dir: string) => void
}) {
  const [byProject, setByProject] = useState<Record<string, SessionRecent[]>>({})
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())

  const key = props.projects.join('|')
  useEffect(() => {
    let cancelled = false
    Promise.all(
      props.projects.map((p) =>
        window.memcode
          .sessionsRecent(p)
          .then((s) => [p, s] as const)
          .catch(() => [p, [] as SessionRecent[]] as const),
      ),
    ).then((entries) => {
      if (!cancelled) setByProject(Object.fromEntries(entries))
    })
    return () => {
      cancelled = true
    }
  }, [key, props.reloadKey])

  const toggle = (p: string) =>
    setCollapsed((prev) => {
      const next = new Set(prev)
      next.has(p) ? next.delete(p) : next.add(p)
      return next
    })

  const titleOf = (s: SessionRecent) => s.title || s.task || 'Untitled'

  return (
    <aside className="sidebar">
      <div className="sidebar-drag" />
      <div className="sidebar-brand">
        <button className="sidebar-title" onClick={props.onSettings} title="Account and settings">
          Memcode
        </button>
        <button className="sidebar-icon" type="button" onClick={props.onSettings} title="Account and settings">
          <Icon name="chevron-down" />
        </button>
      </div>
      <div className="sidebar-scroll">
        <nav className="sidebar-nav" aria-label="Primary">
          <div className="sidebar-section-title">Workspace</div>
          <button className="nav-item" type="button" onClick={props.onNew}>
            <Icon name="compose" />
            <span>New chat</span>
          </button>
          <button className={`nav-item ${props.settingsOpen ? 'active' : ''}`} type="button" onClick={props.onSettings}>
            <Icon name="settings" />
            <span>Settings</span>
          </button>
        </nav>

        <div className="sidebar-section-title projects-section">Projects</div>
        {props.projects.length === 0 && <div className="sidebar-empty">No folder open. Press ⌘O to choose one.</div>}
        {props.projects.map((p) => {
          const name = p.split('/').pop()
          const sessions = (byProject[p] || []).filter((s) => s.resumable)
          const isCollapsed = collapsed.has(p)
          const isActive = p === props.activeRepo
          return (
            <div className="proj" key={p}>
              <button
                className={`proj-head ${isActive ? 'active' : ''}`}
                title={p}
                onClick={() => {
                  if (!isActive) props.onSelectProject(p)
                  toggle(p)
                }}
              >
                <span className={`proj-caret ${isCollapsed ? '' : 'open'}`}>
                  <Icon name="chevron-right" />
                </span>
                <span className="proj-folder">
                  <Icon name="folder" />
                </span>
                <span className="proj-name">{name}</span>
              </button>
              {!isCollapsed && (
                <div className="proj-sessions">
                  {sessions.length === 0 && <div className="sidebar-empty small">No conversations yet.</div>}
                  {sessions.map((s) => (
                    <button
                      key={s.id}
                      className={`sidebar-item ${s.id === props.activeId ? 'active' : ''}`}
                      title={titleOf(s)}
                      onClick={() => props.onResume(p, s.id)}
                    >
                      {titleOf(s)}
                    </button>
                  ))}
                </div>
              )}
            </div>
          )
        })}
        <div className="sidebar-section-title recents-section">Recents</div>
        <div className="sidebar-empty">No archived chats.</div>
      </div>
    </aside>
  )
}

function Icon(props: { name: IconName }) {
  const common = {
    viewBox: '0 0 24 24',
    fill: 'none',
    stroke: 'currentColor',
    strokeWidth: 1.9,
    strokeLinecap: 'round' as const,
    strokeLinejoin: 'round' as const,
    'aria-hidden': true,
  }

  switch (props.name) {
    case 'chevron-down':
      return (
        <svg {...common}>
          <path d="m6 9 6 6 6-6" />
        </svg>
      )
    case 'chevron-right':
      return (
        <svg {...common}>
          <path d="m9 18 6-6-6-6" />
        </svg>
      )
    case 'compose':
      return (
        <svg {...common}>
          <path d="M12 20h9" />
          <path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L8 18l-4 1 1-4Z" />
        </svg>
      )
    case 'folder':
      return (
        <svg {...common}>
          <path d="M3 7.5a2 2 0 0 1 2-2h5l2 2h7a2 2 0 0 1 2 2v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z" />
        </svg>
      )
    case 'settings':
      return (
        <svg {...common}>
          <path d="M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Z" />
          <path d="M19.4 15a1.7 1.7 0 0 0 .34 1.88l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06A1.7 1.7 0 0 0 15 19.4a1.7 1.7 0 0 0-1 .6 1.7 1.7 0 0 0-.4 1.1V21a2 2 0 1 1-4 0v-.09A1.7 1.7 0 0 0 8.6 19.4a1.7 1.7 0 0 0-1.88.34l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.7 1.7 0 0 0 4.6 15a1.7 1.7 0 0 0-.6-1 1.7 1.7 0 0 0-1.1-.4H2.8a2 2 0 1 1 0-4h.09A1.7 1.7 0 0 0 4.6 8.6a1.7 1.7 0 0 0-.34-1.88l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.7 1.7 0 0 0 9 4.6a1.7 1.7 0 0 0 1-.6 1.7 1.7 0 0 0 .4-1.1V2.8a2 2 0 1 1 4 0v.09A1.7 1.7 0 0 0 15 4.6a1.7 1.7 0 0 0 1.88-.34l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.7 1.7 0 0 0 19.4 9a1.7 1.7 0 0 0 .6 1 1.7 1.7 0 0 0 1.1.4h.09a2 2 0 1 1 0 4h-.09A1.7 1.7 0 0 0 19.4 15Z" />
        </svg>
      )
  }
}

type IconName = 'chevron-down' | 'chevron-right' | 'compose' | 'folder' | 'settings'
