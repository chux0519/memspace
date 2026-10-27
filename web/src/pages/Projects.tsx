import { api } from '../api'
import { Async, Card, Empty, SessionRow, Stat, Time, useAsync } from '../components'
import { href } from '../router'
import { num } from '../util'

export function Projects() {
  const state = useAsync(api.projects, [])
  return (
    <div className="page">
      <header className="page-head">
        <h1>项目</h1>
      </header>
      <Async state={state}>
        {(projects) =>
          projects.length === 0 ? (
            <Empty>还没有项目。</Empty>
          ) : (
            <Card>
              <div className="list">
                {projects.map((p) => (
                  <a key={p.project_id} className="row" href={href('projects/' + p.project_id)}>
                    <div className="row-main">
                      <div className="row-title">{p.name}</div>
                      <div className="row-meta mono">{p.canonical_git_identity ?? '（无 git 远端，按目录名识别）'}</div>
                    </div>
                    <div className="row-side">
                      <Time iso={p.updated_at} />
                    </div>
                  </a>
                ))}
              </div>
            </Card>
          )
        }
      </Async>
    </div>
  )
}

export function ProjectPage({ id }: { id: string }) {
  const state = useAsync(() => api.project(id), [id])
  return (
    <div className="page">
      <Async state={state}>
        {(d) => (
          <>
            <header className="page-head">
              <div>
                <div className="crumbs">
                  <a href={href('projects')}>项目</a> /
                </div>
                <h1>{d.project.name}</h1>
                <div className="mono muted">{d.project.canonical_git_identity ?? '无 git 远端'}</div>
              </div>
              <div className="head-actions">
                <a className="btn" href={href('search', { project: id })}>在此项目中搜索</a>
                <a className="btn-ghost" href={href('memories', { project: id })}>记忆</a>
              </div>
            </header>
            <div className="stats">
              <Stat label="会话" value={num(d.stats.sessions)} link={href('sessions', { project: id })} />
              <Stat label="事件" value={num(d.stats.events)} />
              <Stat label="记忆" value={num(d.stats.memories)} link={href('memories', { project: id })} />
              <Stat label="最后活动" value={<Time iso={d.stats.last_activity_at} />} />
            </div>
            <Card title="各设备上的位置">
              {d.locations.length === 0 ? (
                <Empty>没有位置记录。</Empty>
              ) : (
                <div className="table-wrap">
                  <table className="table">
                    <thead>
                      <tr>
                        <th>设备</th>
                        <th>路径</th>
                        <th>分支</th>
                        <th>HEAD</th>
                        <th>最后观察</th>
                      </tr>
                    </thead>
                    <tbody>
                      {d.locations.map((l) => (
                        <tr key={l.device_id + l.absolute_path}>
                          <td className="strong">{l.device_name || l.device_id}</td>
                          <td className="mono">{l.absolute_path}</td>
                          <td className="mono">{l.git_branch || '—'}</td>
                          <td className="mono">{l.git_head ? l.git_head.slice(0, 10) : '—'}</td>
                          <td>
                            <Time iso={l.last_observed_at} />
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </Card>
            <Card title="会话" actions={<a href={href('sessions', { project: id })}>全部 →</a>}>
              {d.sessions.length === 0 ? (
                <Empty>没有会话。</Empty>
              ) : (
                <div className="list">
                  {d.sessions.map((s) => (
                    <SessionRow key={s.session_id} s={s} hideProject />
                  ))}
                </div>
              )}
            </Card>
          </>
        )}
      </Async>
    </div>
  )
}
