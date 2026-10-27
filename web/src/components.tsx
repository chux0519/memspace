import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import type { EventRecord, Memory, SessionInfo } from './api'
import { href } from './router'
import {
  agentLabel, clockTime, eventLabel, eventTone, headline, memoryTypeLabel, oneLine, relTime, statusLabel, absTime,
} from './util'

// useAsync runs load whenever deps change and keeps the latest result.
export function useAsync<T>(load: () => Promise<T>, deps: unknown[]) {
  const [state, setState] = useState<{ data?: T; error?: Error; loading: boolean }>({ loading: true })
  const seq = useRef(0)
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const run = useCallback(load, deps)
  const reload = useCallback(() => {
    const n = ++seq.current
    setState((s) => ({ ...s, loading: true, error: undefined }))
    run().then(
      (data) => n === seq.current && setState({ data, loading: false }),
      (error: Error) => n === seq.current && setState({ error, loading: false }),
    )
  }, [run])
  useEffect(reload, [reload])
  return { ...state, reload }
}

export function Loading() {
  return <div className="loading">加载中…</div>
}

export function ErrorBox({ error }: { error: Error }) {
  return <div className="error-box">出错了：{error.message}</div>
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>
}

export function Async<T>({ state, children }: { state: { data?: T; error?: Error; loading: boolean }; children: (d: T) => ReactNode }) {
  if (state.error) return <ErrorBox error={state.error} />
  if (state.data === undefined) return <Loading />
  return <>{children(state.data)}</>
}

export function Badge({ tone = 'neutral', children, title }: { tone?: string; children: ReactNode; title?: string }) {
  return (
    <span className={'badge badge-' + tone} title={title}>
      {children}
    </span>
  )
}

export function StatusBadge({ status }: { status: string }) {
  const tone = status === 'active' ? 'live' : status === 'interrupted' ? 'warn' : status === 'retracted' ? 'bad' : 'neutral'
  return <Badge tone={tone}>{statusLabel[status] ?? status}</Badge>
}

export function AgentBadge({ agent }: { agent: string }) {
  const tone = agent === 'codex' ? 'codex' : agent === 'dsh' ? 'dsh' : 'claude'
  return <Badge tone={tone}>{agentLabel[agent] ?? agent}</Badge>
}

export function Time({ iso, abs }: { iso?: string; abs?: boolean }) {
  return (
    <time dateTime={iso} title={absTime(iso)}>
      {abs ? absTime(iso) : relTime(iso)}
    </time>
  )
}

export function Card({ title, actions, children, className }: { title?: ReactNode; actions?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={'card ' + (className ?? '')}>
      {(title || actions) && (
        <header className="card-head">
          {title && <h2>{title}</h2>}
          {actions && <div className="card-actions">{actions}</div>}
        </header>
      )}
      {children}
    </section>
  )
}

export function SessionRow({ s, hideProject }: { s: SessionInfo; hideProject?: boolean }) {
  const text = s.latest_request && s.latest_request !== s.objective ? s.latest_request : ''
  return (
    <a className="row session-row" href={href('sessions/' + s.session_id)}>
      <div className="row-main">
        <div className="row-title">{s.objective ? oneLine(s.objective, 120) : <span className="muted">（没有用户请求）</span>}</div>
        {text && <div className="row-sub">最近：{oneLine(text, 120)}</div>}
        <div className="row-meta">
          {!hideProject && <span className="project-name">{s.project_name}</span>}
          <AgentBadge agent={s.agent_type} />
          <span>{s.device_name || '未知设备'}</span>
          <span>{s.event_count} 个事件</span>
        </div>
      </div>
      <div className="row-side">
        <StatusBadge status={s.status} />
        <Time iso={s.last_event_at ?? s.started_at} />
      </div>
    </a>
  )
}

export function MemoryRow({ m, hideProject }: { m: Memory; hideProject?: boolean }) {
  return (
    <a className="row" href={href('memories/' + m.memory_id)}>
      <div className="row-main">
        <div className="row-title">{oneLine(m.title, 140)}</div>
        <div className="row-sub">{oneLine(m.content, 200)}</div>
        <div className="row-meta">
          <Badge tone={m.type === 'procedural' ? 'file' : 'neutral'}>{memoryTypeLabel[m.type] ?? m.type}</Badge>
          {!hideProject && m.project_name && <span className="project-name">{m.project_name}</span>}
          <span title="置信度">置信 {Math.round(m.confidence * 100)}%</span>
          <span>{m.source_refs.length} 个来源</span>
        </div>
      </div>
      <div className="row-side">
        <StatusBadge status={m.status} />
        <Time iso={m.updated_at} />
      </div>
    </a>
  )
}

// Highlight renders a search snippet whose matches are wrapped in U+E000 …
// U+E001 (requested with highlight: "marks"). Text is never parsed as HTML.
export function Highlight({ text }: { text: string }) {
  const parts: ReactNode[] = []
  let rest = text
  let key = 0
  for (;;) {
    const i = rest.indexOf('')
    if (i < 0) break
    const j = rest.indexOf('', i + 1)
    if (j < 0) break
    if (i > 0) parts.push(rest.slice(0, i))
    parts.push(<mark key={key++}>{rest.slice(i + 1, j)}</mark>)
    rest = rest.slice(j + 1)
  }
  if (rest) parts.push(rest.replace(/[]/g, ''))
  return <>{parts}</>
}

export function JSONView({ value }: { value: unknown }) {
  return <pre className="json">{JSON.stringify(value, null, 2)}</pre>
}

const longFields = ['prompt', 'last_assistant_message', 'tool_response', 'error', 'output', 'stdout', 'stderr']

// EventBody shows the important text of an event in full, then the raw payload.
export function EventBody({ e }: { e: EventRecord }) {
  const p = e.payload ?? {}
  const ti = (p.tool_input && typeof p.tool_input === 'object' ? p.tool_input : {}) as Record<string, unknown>
  const blocks: { label: string; text: string }[] = []
  if (typeof ti.command === 'string') blocks.push({ label: '命令', text: ti.command })
  for (const f of longFields) {
    const v = p[f]
    if (typeof v === 'string' && v.trim()) blocks.push({ label: f, text: v })
    else if (v && typeof v === 'object') blocks.push({ label: f, text: JSON.stringify(v, null, 2) })
  }
  return (
    <div className="event-body">
      {blocks.map((b, i) => (
        <div key={i} className="event-block">
          <div className="event-block-label">{b.label}</div>
          <pre className="text">{b.text}</pre>
        </div>
      ))}
      <details>
        <summary>原始 payload</summary>
        <JSONView value={p} />
      </details>
    </div>
  )
}

export function EventLine({ e, open, onToggle, focus }: { e: EventRecord; open: boolean; onToggle: () => void; focus?: boolean }) {
  const tone = eventTone(e.event_type)
  const line = headline(e)
  return (
    <li className={'event ev-' + tone + (focus ? ' focus' : '')} id={'ev-' + e.event_id}>
      <button className="event-line" onClick={onToggle} aria-expanded={open}>
        <span className="event-time">{clockTime(e.occurred_at)}</span>
        <span className="event-dot" />
        <span className="event-type">{eventLabel[e.event_type] ?? e.event_type}</span>
        <span className="event-text">{line ? oneLine(line, 220) : <span className="muted">—</span>}</span>
      </button>
      {open && (
        <div className="event-detail">
          <div className="event-detail-meta">
            <a href={href('events/' + e.event_id)}>{e.event_id}</a>
            <span>{absTime(e.occurred_at)}</span>
          </div>
          <EventBody e={e} />
        </div>
      )}
    </li>
  )
}

export function Stat({ label, value, sub, link }: { label: string; value: ReactNode; sub?: ReactNode; link?: string }) {
  const body = (
    <>
      <div className="stat-value">{value}</div>
      <div className="stat-label">{label}</div>
      {sub && <div className="stat-sub">{sub}</div>}
    </>
  )
  return link ? (
    <a className="stat" href={link}>
      {body}
    </a>
  ) : (
    <div className="stat">{body}</div>
  )
}

export function Select({ value, onChange, options, label }: {
  value: string; onChange: (v: string) => void; options: [string, string][]; label: string
}) {
  return (
    <label className="field">
      <span>{label}</span>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        {options.map(([v, l]) => (
          <option key={v} value={v}>
            {l}
          </option>
        ))}
      </select>
    </label>
  )
}
