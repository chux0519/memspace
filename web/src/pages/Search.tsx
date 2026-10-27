import { useEffect, useState, type FormEvent } from 'react'
import { api, type SearchResponse, type SearchResult } from '../api'
import { AgentBadge, Badge, Card, Empty, ErrorBox, Highlight, Loading, Select, Time, useAsync } from '../components'
import { href, navigate, type Route } from '../router'
import { kindLabel } from '../util'

const sinceOptions: [string, string][] = [
  ['', '不限时间'],
  ['1', '24 小时'],
  ['7', '7 天'],
  ['30', '30 天'],
  ['90', '90 天'],
]

function resultLink(r: SearchResult): string {
  if (r.kind === 'memory') return href('memories/' + r.ref_id)
  if (r.kind === 'summary') return href('sessions/' + r.ref_id)
  if (r.session_id) return href('sessions/' + r.session_id, { event: r.ref_id })
  return href('events/' + r.ref_id)
}

function matchBadge(r: SearchResult) {
  if (r.match === 'semantic') return <Badge tone="agent" title="语义相近，未必包含关键词">语义 {Math.round((r.similarity ?? 0) * 100)}%</Badge>
  if (r.match === 'any') return <Badge tone="neutral" title="部分关键词命中">命中 {r.matched_terms}/{r.total_terms}</Badge>
  return null
}

export function Search({ route }: { route: Route }) {
  const p = route.params
  const query = p.get('q') ?? ''
  const project = p.get('project') ?? ''
  const kind = p.get('kind') ?? ''
  const agent = p.get('agent') ?? ''
  const mode = p.get('mode') ?? ''
  const since = p.get('since') ?? ''
  const [draft, setDraft] = useState(query)
  useEffect(() => setDraft(query), [query])
  const projects = useAsync(api.projects, [])

  const [res, setRes] = useState<{ data?: SearchResponse; error?: Error; loading: boolean }>({ loading: false })
  useEffect(() => {
    if (!query.trim()) {
      setRes({ loading: false })
      return
    }
    let live = true
    setRes((r) => ({ ...r, loading: true, error: undefined }))
    api
      .search({
        query,
        scope: project ? 'selected_projects' : 'all_projects',
        project_ids: project ? [project] : undefined,
        kinds: kind ? [kind] : undefined,
        agent_types: agent ? [agent] : undefined,
        mode: mode || undefined,
        since: since ? new Date(Date.now() - Number(since) * 86400e3).toISOString() : undefined,
        limit: 50,
      })
      .then(
        (data) => live && setRes({ data, loading: false }),
        (error: Error) => live && setRes({ error, loading: false }),
      )
    return () => {
      live = false
    }
  }, [query, project, kind, agent, mode, since])

  const params = { q: query, project, kind, agent, mode, since }
  const set = (k: string, v: string) => navigate('search', { ...params, [k]: v }, true)
  const submit = (e: FormEvent) => {
    e.preventDefault()
    navigate('search', { ...params, q: draft.trim() })
  }

  return (
    <div className="page">
      <header className="page-head">
        <h1>搜索</h1>
      </header>
      <form className="search-form" onSubmit={submit} role="search">
        <input type="search" value={draft} onChange={(e) => setDraft(e.target.value)} placeholder="中文或英文，例如：认证超时、Stripe webhook 幂等" autoFocus aria-label="搜索内容" />
        <button className="btn" type="submit">搜索</button>
      </form>
      <div className="filters">
        <Select label="项目" value={project} onChange={(v) => set('project', v)}
          options={[['', '全部项目'], ...(projects.data ?? []).map((x): [string, string] => [x.project_id, x.name])]} />
        <Select label="类型" value={kind} onChange={(v) => set('kind', v)}
          options={[['', '全部'], ['prompt', '用户请求'], ['reply', '回复'], ['failure', '失败'], ['command', '命令'], ['change', '文件修改'], ['summary', '会话摘要'], ['memory', '记忆']]} />
        <Select label="Agent" value={agent} onChange={(v) => set('agent', v)}
          options={[['', '全部'], ['claude-code', 'Claude Code'], ['codex', 'Codex'], ['dsh', 'DSH']]} />
        <Select label="时间" value={since} onChange={(v) => set('since', v)} options={sinceOptions} />
        <Select label="方式" value={mode} onChange={(v) => set('mode', v)}
          options={[['', '混合（默认）'], ['keyword', '仅关键词'], ['semantic', '仅语义']]} />
      </div>
      {res.error && <ErrorBox error={res.error} />}
      {res.loading && <Loading />}
      {!query && <Empty>输入关键词搜索所有设备上的会话、事件、摘要和记忆。</Empty>}
      {res.data && !res.loading && (
        <Card>
          {res.data.note && <p className="hint">{res.data.note}</p>}
          {res.data.results.length === 0 ? (
            <Empty>没有结果。</Empty>
          ) : (
            <div className="list">
              {res.data.results.map((r) => (
                <a key={r.doc_id} className="row result" href={resultLink(r)}>
                  <div className="row-main">
                    <div className="row-meta">
                      <Badge tone={r.kind === 'memory' ? 'file' : r.kind === 'summary' ? 'agent' : r.kind === 'failure' ? 'bad' : 'neutral'}>{kindLabel[r.kind] ?? r.kind}</Badge>
                      {r.title && <span className="strong">{r.title}</span>}
                      {matchBadge(r)}
                    </div>
                    {r.snippet && (
                      <div className="snippet">
                        <Highlight text={r.snippet} />
                      </div>
                    )}
                    <div className="row-meta">
                      <span className="project-name">{r.project_name}</span>
                      {r.agent_type && <AgentBadge agent={r.agent_type} />}
                      {r.device_name && <span>{r.device_name}</span>}
                    </div>
                  </div>
                  <div className="row-side">
                    <Time iso={r.occurred_at} />
                  </div>
                </a>
              ))}
            </div>
          )}
        </Card>
      )}
    </div>
  )
}
