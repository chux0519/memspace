import { api, type Overview as OverviewData } from '../api'
import { Async, Badge, Card, Empty, MemoryRow, SessionRow, Stat, Time, useAsync } from '../components'
import { href } from '../router'
import { agentLabel, num, oneLine } from '../util'
import { DeviceTable, IndexStatus } from './Devices'

export function Overview() {
  const state = useAsync(api.overview, [])
  return (
    <div className="page">
      <header className="page-head">
        <h1>概览</h1>
        <button className="btn-ghost" onClick={state.reload} disabled={state.loading}>
          刷新
        </button>
      </header>
      <Async state={state}>{(o) => <OverviewBody o={o} />}</Async>
    </div>
  )
}

function OverviewBody({ o }: { o: OverviewData }) {
  const backlog = o.devices.reduce((n, d) => n + (d.spool_pending ?? 0), 0)
  const dead = o.devices.reduce((n, d) => n + (d.spool_dead ?? 0), 0)
  return (
    <>
      <div className="stats">
        <Stat label="项目" value={num(o.counts.projects)} link={href('projects')} />
        <Stat label="设备" value={num(o.counts.devices)} link={href('devices')}
          sub={backlog > 0 ? <span className="warn-text">{num(backlog)} 条待上传</span> : dead > 0 ? <span className="bad-text">{num(dead)} 条上传失败</span> : '全部已同步'} />
        <Stat label="近 7 天会话" value={num(o.counts.sessions_7d)} link={href('sessions')} />
        <Stat label="近 24 小时事件" value={num(o.counts.events_24h)} sub={`共 ${num(o.counts.events_total)}`} />
        <Stat label="记忆" value={num(o.counts.memories)} link={href('memories')} />
      </div>

      <div className="grid-2">
        <Card title="未完成的工作" actions={<span className="muted small">近 3 天</span>}>
          {o.unfinished.length === 0 ? (
            <Empty>没有中断或留有待办的会话。</Empty>
          ) : (
            <div className="list">
              {o.unfinished.map((u) => (
                <a key={u.session.session_id} className="row" href={href('sessions/' + u.session.session_id)}>
                  <div className="row-main">
                    <div className="row-title">{oneLine(u.session.latest_request || u.session.objective || '（没有用户请求）', 110)}</div>
                    {u.next_steps.length > 0 && <div className="row-sub">下一步：{oneLine(u.next_steps[0], 120)}</div>}
                    <div className="row-meta">
                      <span className="project-name">{u.session.project_name}</span>
                      <span>{agentLabel[u.session.agent_type] ?? u.session.agent_type}</span>
                      <span>{u.session.device_name}</span>
                    </div>
                  </div>
                  <div className="row-side">
                    {u.session.status === 'active' ? <Badge tone="live">进行中</Badge> : u.session.status === 'interrupted' ? <Badge tone="warn">中断</Badge> : null}
                    {u.unresolved > 0 && <Badge tone="bad">{u.unresolved} 个未解决</Badge>}
                    <Time iso={u.session.last_event_at} />
                  </div>
                </a>
              ))}
            </div>
          )}
        </Card>

        <Card title="最近会话" actions={<a href={href('sessions')}>全部 →</a>}>
          {o.recent_sessions.length === 0 ? (
            <Empty>还没有会话。在设备上运行 <code>memspace agent install</code> 开始采集。</Empty>
          ) : (
            <div className="list">
              {o.recent_sessions.slice(0, 6).map((s) => (
                <SessionRow key={s.session_id} s={s} />
              ))}
            </div>
          )}
        </Card>
      </div>

      <Card title="设备与同步" actions={<a href={href('devices')}>详情 →</a>}>
        <DeviceTable devices={o.devices} compact />
      </Card>

      <div className="grid-2">
        <Card title="最新记忆" actions={<a href={href('memories')}>全部 →</a>}>
          {o.latest_memories.length === 0 ? (
            <Empty>会话结束并生成摘要后，会在这里出现记忆。</Empty>
          ) : (
            <div className="list">
              {o.latest_memories.map((m) => (
                <MemoryRow key={m.memory_id} m={m} />
              ))}
            </div>
          )}
        </Card>
        <Card title="索引与任务">
          <IndexStatus embedding={o.embedding} jobs={o.jobs} />
        </Card>
      </div>
    </>
  )
}
