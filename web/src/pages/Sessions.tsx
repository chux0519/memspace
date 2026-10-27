import { useEffect, useMemo, useState } from 'react'
import { api, type EventRecord, type SessionInfo, type Summary } from '../api'
import { AgentBadge, Async, Badge, Card, Empty, ErrorBox, EventBody, EventLine, Loading, SessionRow, Select, StatusBadge, Time, useAsync } from '../components'
import { href, navigate, type Route } from '../router'
import { absTime, dayKey, duration, eventLabel, sessionCursor } from '../util'

const PAGE = 50

export function Sessions({ route }: { route: Route }) {
  const project = route.params.get('project') ?? ''
  const agent = route.params.get('agent') ?? ''
  const device = route.params.get('device_id') ?? ''
  const status = route.params.get('status') ?? ''
  const projects = useAsync(api.projects, [])
  const devices = useAsync(api.devices, [])
  const [items, setItems] = useState<SessionInfo[]>([])
  const [more, setMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<Error>()

  const filter = { project_id: project, agent_type: agent, device_id: device, status }
  const load = (after?: SessionInfo) => {
    setLoading(true)
    setError(undefined)
    api.sessions({ ...filter, limit: PAGE, ...(after ? sessionCursor(after) : {}) }).then(
      (page) => {
        setItems((prev) => (after ? [...prev, ...page] : page))
        setMore(page.length === PAGE)
        setLoading(false)
      },
      (e: Error) => {
        setError(e)
        setLoading(false)
      },
    )
  }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => load(), [project, agent, device, status])

  const set = (k: string, v: string) => {
    const next: Record<string, string> = { project, agent, device_id: device, status }
    next[k] = v
    navigate('sessions', next, true)
  }

  return (
    <div className="page">
      <header className="page-head">
        <h1>会话</h1>
      </header>
      <div className="filters">
        <Select label="项目" value={project} onChange={(v) => set('project', v)}
          options={[['', '全部项目'], ...(projects.data ?? []).map((p): [string, string] => [p.project_id, p.name])]} />
        <Select label="Agent" value={agent} onChange={(v) => set('agent', v)}
          options={[['', '全部'], ['claude-code', 'Claude Code'], ['codex', 'Codex']]} />
        <Select label="设备" value={device} onChange={(v) => set('device_id', v)}
          options={[['', '全部设备'], ...(devices.data ?? []).map((d): [string, string] => [d.device_id, d.name])]} />
        <Select label="状态" value={status} onChange={(v) => set('status', v)}
          options={[['', '全部'], ['active', '进行中'], ['ended', '已结束'], ['interrupted', '中断']]} />
      </div>
      {error && <ErrorBox error={error} />}
      <Card>
        {items.length === 0 && !loading ? (
          <Empty>没有符合条件的会话。</Empty>
        ) : (
          <div className="list">
            {items.map((s) => (
              <SessionRow key={s.session_id} s={s} />
            ))}
          </div>
        )}
        {loading && <Loading />}
        {more && !loading && (
          <button className="load-more" onClick={() => load(items[items.length - 1])}>
            加载更多
          </button>
        )}
      </Card>
    </div>
  )
}

export function SessionPage({ id, route }: { id: string; route: Route }) {
  const focus = route.params.get('event') ?? ''
  const detail = useAsync(() => api.sessionDetail(id), [id])
  return (
    <div className="page">
      <Async state={detail}>
        {(d) => (
          <>
            <header className="page-head">
              <div>
                <div className="crumbs">
                  <a href={href('sessions')}>会话</a> / <a href={href('projects/' + d.session.project_id)}>{d.session.project_name}</a> /
                </div>
                <h1 className="session-title">{d.summary?.objective || d.session.objective || '（没有用户请求）'}</h1>
                <div className="row-meta">
                  <StatusBadge status={d.session.status} />
                  <AgentBadge agent={d.session.agent_type} />
                  <a href={href('sessions', { device_id: d.session.device_id })}>{d.session.device_name}</a>
                  <span>{absTime(d.session.started_at)}</span>
                  {d.summary && d.summary.event_count > 0 && <span>持续 {duration(d.summary.first_event_at, d.summary.last_event_at)}</span>}
                  <span>{d.session.event_count} 个事件</span>
                  <span className="mono muted" title="原生会话 ID">{d.session.native_session_id}</span>
                </div>
              </div>
            </header>
            <div className="session-layout">
              <Timeline id={id} focus={focus} />
              {d.summary && <SummaryPanel s={d.summary} sessionId={id} />}
            </div>
          </>
        )}
      </Async>
    </div>
  )
}

function SummaryPanel({ s, sessionId }: { s: Summary; sessionId: string }) {
  const ev = (eid: string) => href('sessions/' + sessionId, { event: eid })
  return (
    <aside className="summary">
      <Card title="会话摘要" actions={<span className="muted small" title={s.generator}>规则生成</span>}>
        {s.latest_request && s.latest_request !== s.objective && (
          <section>
            <h3>最近的请求</h3>
            <p className="pre-wrap">{s.latest_request}</p>
          </section>
        )}
        {s.completed_work && (
          <section>
            <h3>完成的工作</h3>
            <p className="pre-wrap">{s.completed_work}</p>
          </section>
        )}
        {s.next_steps.length > 0 && (
          <section>
            <h3>下一步</h3>
            <ul>{s.next_steps.map((n, i) => <li key={i}>{n}</li>)}</ul>
          </section>
        )}
        {s.unresolved_issues.length > 0 && (
          <section>
            <h3>未解决</h3>
            <ul>
              {s.unresolved_issues.map((u) => (
                <li key={u.event_id}>
                  <a href={ev(u.event_id)}>{u.title}</a>
                  {u.error && <div className="muted small mono clamp">{u.error}</div>}
                </li>
              ))}
            </ul>
          </section>
        )}
        {s.fixes && s.fixes.length > 0 && (
          <section>
            <h3>修复</h3>
            <ul>
              {s.fixes.map((f) => (
                <li key={f.failed_event_id}>
                  <a href={ev(f.fixed_event_id)}>{f.title}</a>
                  <div className="muted small">
                    <a href={ev(f.failed_event_id)}>失败</a> → 修改 {f.files.length} 个文件 → 通过
                  </div>
                </li>
              ))}
            </ul>
          </section>
        )}
        {s.test_results.length > 0 && (
          <section>
            <h3>测试</h3>
            <ul>
              {s.test_results.map((t) => (
                <li key={t.event_id}>
                  <Badge tone={t.passed ? 'ok' : 'bad'}>{t.passed ? '通过' : '失败'}</Badge>{' '}
                  <a className="mono small" href={ev(t.event_id)}>{t.command}</a>
                </li>
              ))}
            </ul>
          </section>
        )}
        {s.changed_files.length > 0 && (
          <section>
            <h3>修改的文件（{s.changed_files.length}）</h3>
            <ul className="files">{s.changed_files.map((f) => <li key={f} className="mono small">{f}</li>)}</ul>
          </section>
        )}
        <p className="disclaimer">{s.disclaimer}</p>
      </Card>
    </aside>
  )
}

const typeFilters: [string, string][] = [
  ['conversation', '对话'],
  ['tool', '工具'],
  ['file', '文件'],
  ['failed', '失败'],
  ['meta', '会话'],
]

function group(type: string): string {
  if (type === 'user.prompt' || type.startsWith('agent.')) return 'conversation'
  if (type === 'tool.failed') return 'failed'
  if (type === 'file.modified') return 'file'
  if (type.startsWith('tool.')) return 'tool'
  return 'meta'
}

function Timeline({ id, focus }: { id: string; focus: string }) {
  const [events, setEvents] = useState<EventRecord[]>([])
  const [next, setNext] = useState<string | undefined>()
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<Error>()
  const [open, setOpen] = useState<Set<string>>(() => new Set(focus ? [focus] : []))
  const [hidden, setHidden] = useState<Set<string>>(new Set())

  const load = (cursor?: string) => {
    setLoading(true)
    api.sessionEvents(id, cursor).then(
      (p) => {
        setEvents((prev) => (cursor ? [...prev, ...p.events] : p.events))
        setNext(p.next)
        setLoading(false)
      },
      (e: Error) => {
        setError(e)
        setLoading(false)
      },
    )
  }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => load(), [id])

  // Keep loading pages until the focused event is present, then scroll to it.
  useEffect(() => {
    if (!focus || loading) return
    if (events.some((e) => e.event_id === focus)) {
      setOpen((o) => new Set(o).add(focus))
      requestAnimationFrame(() => document.getElementById('ev-' + focus)?.scrollIntoView({ block: 'center' }))
    } else if (next) {
      load(next)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focus, loading, events.length])

  const counts = useMemo(() => {
    const c: Record<string, number> = {}
    for (const e of events) c[group(e.event_type)] = (c[group(e.event_type)] ?? 0) + 1
    return c
  }, [events])

  const visible = events.filter((e) => !hidden.has(group(e.event_type)))
  const toggle = (eid: string) =>
    setOpen((o) => {
      const n = new Set(o)
      if (n.has(eid)) n.delete(eid)
      else n.add(eid)
      return n
    })

  let lastDay = ''
  return (
    <Card title="事件时间线" className="timeline-card"
      actions={
        <div className="chips">
          {typeFilters.map(([k, label]) => (
            <button key={k} className={'chip' + (hidden.has(k) ? '' : ' on')}
              onClick={() => setHidden((h) => { const n = new Set(h); if (n.has(k)) n.delete(k); else n.add(k); return n })}>
              {label} {counts[k] ?? 0}
            </button>
          ))}
        </div>
      }>
      {error && <ErrorBox error={error} />}
      {events.length === 0 && !loading && !error && <Empty>这个会话没有事件。</Empty>}
      <ol className="timeline">
        {visible.map((e) => {
          const day = dayKey(e.occurred_at)
          const header = day !== lastDay ? <li className="day" key={'d' + day}>{day}</li> : null
          lastDay = day
          return [
            header,
            <EventLine key={e.event_id} e={e} open={open.has(e.event_id)} onToggle={() => toggle(e.event_id)} focus={e.event_id === focus} />,
          ]
        })}
      </ol>
      {loading && <Loading />}
      {next && !loading && (
        <button className="load-more" onClick={() => load(next)}>
          加载更多事件
        </button>
      )}
    </Card>
  )
}

export function EventPage({ id }: { id: string }) {
  const state = useAsync(() => api.event(id), [id])
  return (
    <div className="page">
      <Async state={state}>
        {(d) => (
          <>
            <header className="page-head">
              <div>
                <div className="crumbs">
                  事件 /
                </div>
                <h1>{eventLabel[d.event.event_type] ?? d.event.event_type}</h1>
                <div className="row-meta">
                  <span className="mono">{d.event.event_id}</span>
                  <Time iso={d.event.occurred_at} abs />
                </div>
              </div>
              {d.session && (
                <div className="head-actions">
                  <a className="btn" href={href('sessions/' + d.session.session_id, { event: d.event.event_id })}>在时间线中查看</a>
                </div>
              )}
            </header>
            {d.session && (
              <Card title="所属会话">
                <SessionRow s={d.session} />
              </Card>
            )}
            <Card title="内容">
              <EventBody e={d.event} />
            </Card>
          </>
        )}
      </Async>
    </div>
  )
}
