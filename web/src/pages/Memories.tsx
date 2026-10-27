import { useEffect, useState } from 'react'
import { api, type Memory } from '../api'
import { Async, Badge, Card, Empty, ErrorBox, Loading, MemoryRow, Select, StatusBadge, Time, useAsync } from '../components'
import { href, navigate, type Route } from '../router'
import { absTime, eventLabel, memoryTypeLabel, oneLine } from '../util'

const PAGE = 50

export function Memories({ route }: { route: Route }) {
  const project = route.params.get('project') ?? ''
  const type = route.params.get('type') ?? ''
  const projects = useAsync(api.projects, [])
  const [items, setItems] = useState<Memory[]>([])
  const [more, setMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<Error>()

  const load = (after?: Memory) => {
    setLoading(true)
    setError(undefined)
    api
      .memories({ project_id: project, type, limit: PAGE, ...(after ? { before: after.updated_at, before_id: after.memory_id } : {}) })
      .then(
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
  useEffect(() => load(), [project, type])

  const set = (k: string, v: string) => navigate('memories', { project, type, [k]: v }, true)

  return (
    <div className="page">
      <header className="page-head">
        <h1>记忆</h1>
      </header>
      <p className="hint">
        记忆由会话摘要按规则生成（情景：一次会话做了什么；修复：失败 → 改文件 → 通过）。每条都保留来源，可以追溯到原始事件。
      </p>
      <div className="filters">
        <Select label="项目" value={project} onChange={(v) => set('project', v)}
          options={[['', '全部项目'], ...(projects.data ?? []).map((x): [string, string] => [x.project_id, x.name])]} />
        <Select label="类型" value={type} onChange={(v) => set('type', v)}
          options={[['', '全部'], ['episodic', '情景'], ['procedural', '流程 / 修复']]} />
      </div>
      {error && <ErrorBox error={error} />}
      <Card>
        {items.length === 0 && !loading ? (
          <Empty>没有记忆。</Empty>
        ) : (
          <div className="list">
            {items.map((m) => (
              <MemoryRow key={m.memory_id} m={m} />
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

export function MemoryPage({ id }: { id: string }) {
  const state = useAsync(() => api.memory(id), [id])
  return (
    <div className="page">
      <Async state={state}>
        {({ memory: m, sources }) => (
          <>
            <header className="page-head">
              <div>
                <div className="crumbs">
                  <a href={href('memories')}>记忆</a> / <a href={href('projects/' + m.project_id)}>{m.project_name}</a> /
                </div>
                <h1>{m.title}</h1>
                <div className="row-meta">
                  <Badge tone={m.type === 'procedural' ? 'file' : 'neutral'}>{memoryTypeLabel[m.type] ?? m.type}</Badge>
                  <StatusBadge status={m.status} />
                  <span>置信 {Math.round(m.confidence * 100)}%</span>
                  <span>更新于 {absTime(m.updated_at)}</span>
                  <span className="muted small" title="生成器">{m.generator}</span>
                </div>
              </div>
              {m.session_id && (
                <div className="head-actions">
                  <a className="btn" href={href('sessions/' + m.session_id)}>查看会话</a>
                </div>
              )}
            </header>
            <Card title="内容">
              <p className="pre-wrap">{m.content}</p>
              {m.status === 'candidate' && <p className="disclaimer">候选记忆：由规则自动推断，尚未经人确认。</p>}
            </Card>
            <Card title={`来源（${sources.length}）`}>
              {sources.length === 0 ? (
                <Empty>没有来源记录。</Empty>
              ) : (
                <div className="list">
                  {sources.map((s, i) => {
                    const link = s.missing ? undefined : s.kind === 'session' ? href('sessions/' + s.id)
                      : s.session_id ? href('sessions/' + s.session_id, { event: s.id }) : href('events/' + s.id)
                    const body = (
                      <>
                        <div className="row-main">
                          <div className="row-meta">
                            <Badge>{s.kind === 'session' ? '会话' : eventLabel[s.event_type ?? ''] ?? s.event_type ?? '事件'}</Badge>
                            <span className="mono small">{s.id}</span>
                            {s.missing && <Badge tone="bad">已不存在</Badge>}
                          </div>
                          {s.summary && <div className="row-sub">{oneLine(s.summary, 200)}</div>}
                        </div>
                        <div className="row-side">{s.occurred_at && <Time iso={s.occurred_at} />}</div>
                      </>
                    )
                    return link ? (
                      <a key={i} className="row" href={link}>{body}</a>
                    ) : (
                      <div key={i} className="row">{body}</div>
                    )
                  })}
                </div>
              )}
            </Card>
          </>
        )}
      </Async>
    </div>
  )
}

