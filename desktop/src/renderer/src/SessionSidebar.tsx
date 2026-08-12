import { useEffect, useState } from 'react'
import type { SessionRecent } from '../../shared/cli-types'

// Session history for the open repo (from `memcode session recent --json`).
// Click a resumable session to reopen its thread; New starts a fresh one.
export function SessionSidebar(props: {
  cwd: string | null
  reloadKey: number // bump to refetch (e.g. after a turn completes)
  activeId: string | null
  onResume: (id: string) => void
  onNew: () => void
}) {
  const [sessions, setSessions] = useState<SessionRecent[]>([])

  useEffect(() => {
    if (!props.cwd) {
      setSessions([])
      return
    }
    window.memcode.sessionsRecent(props.cwd).then(setSessions).catch(() => setSessions([]))
  }, [props.cwd, props.reloadKey])

  if (!props.cwd) {
    return (
      <aside className="sidebar">
        <div className="sidebar-title">Sessions</div>
        <div className="sidebar-empty">No folder open.</div>
      </aside>
    )
  }

  return (
    <aside className="sidebar">
      <button className="sidebar-new" onClick={props.onNew}>
        <span className="plus">+</span> New session
      </button>
      <div className="sidebar-title">Sessions</div>
      <div className="sidebar-list">
        {sessions.length === 0 && <div className="sidebar-empty">No sessions yet.</div>}
        {sessions.map((s) => (
          <button
            key={s.id}
            className={`sidebar-item ${s.id === props.activeId ? 'active' : ''}`}
            disabled={!s.resumable}
            title={s.resumable ? 'Resume this session' : 'No saved transcript to resume'}
            onClick={() => s.resumable && props.onResume(s.id)}
          >
            <span className="si-task">{s.task || '(untitled)'}</span>
            <span className="si-meta">
              {s.mode} · {s.files_changed} file{s.files_changed === 1 ? '' : 's'}
              {!s.resumable && ' · read-only'}
            </span>
          </button>
        ))}
      </div>
    </aside>
  )
}
