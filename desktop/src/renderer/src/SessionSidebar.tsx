import { useEffect, useState } from 'react'
import type { SessionRecent } from '../../shared/cli-types'

// Sidebar grouped by project (recent folders), each with its resumable chats
// nested under it — like a normal chat app. Titles come from the CLI (generated
// server-side); only resumable conversations are listed and clickable.
export function SessionSidebar(props: {
  projects: string[]
  activeRepo: string | null
  activeId: string | null
  reloadKey: number
  onNew: () => void
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
      <button className="sidebar-new" onClick={props.onNew}>
        <span className="plus">+</span> New session
      </button>
      <div className="sidebar-scroll">
        {props.projects.length === 0 && <div className="sidebar-empty">No folder open — ⌘O to open one.</div>}
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
                <span className="proj-caret">{isCollapsed ? '▸' : '▾'}</span>
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
      </div>
    </aside>
  )
}
