import { api, type DeviceStatus, type EmbeddingStatus } from '../api'
import { Async, Badge, Card, Empty, Time, useAsync } from '../components'
import { href } from '../router'
import { newerRelease, num, relTime } from '../util'

export function Devices() {
  const state = useAsync(api.overview, [])
  const health = useAsync(api.health, [])
  return (
    <div className="page">
      <header className="page-head">
        <h1>设备与同步</h1>
        <button className="btn-ghost" onClick={state.reload} disabled={state.loading}>
          刷新
        </button>
      </header>
      <Async state={state}>
        {(o) => (
          <>
            <Card title="设备">
              <DeviceTable devices={o.devices} serverVersion={health.data?.version} />
              <p className="hint">
                “待上传”来自设备每次同步后的报告：仍在本地 spool 中、尚未被服务器确认的事件。设备离线时，这个数字停留在最后一次报告的值。
                客户端比服务器旧时，会在下次同步后从 GitHub 自动升级到服务器的版本；失败时这里会显示原因，可在该设备上运行 <code>memspace upgrade</code>。
              </p>
            </Card>
            <Card title="语义索引与后台任务">
              <IndexStatus embedding={o.embedding} jobs={o.jobs} />
            </Card>
          </>
        )}
      </Async>
    </div>
  )
}

function syncState(d: DeviceStatus): { tone: string; label: string } {
  if (d.revoked_at) return { tone: 'bad', label: '已撤销' }
  if ((d.spool_dead ?? 0) > 0) return { tone: 'bad', label: '有失败' }
  if ((d.spool_pending ?? 0) > 0) return { tone: 'warn', label: '待上传' }
  if (d.spool_reported_at === undefined) return { tone: 'neutral', label: '未报告' }
  return { tone: 'ok', label: '已同步' }
}

export function DeviceTable({ devices, compact, serverVersion }: { devices: DeviceStatus[]; compact?: boolean; serverVersion?: string }) {
  if (devices.length === 0) return <Empty>还没有设备。在服务器上运行 <code>memspace server invite</code>，再在设备上 <code>memspace client connect</code>。</Empty>
  return (
    <div className="table-wrap">
      <table className="table">
        <thead>
          <tr>
            <th>设备</th>
            <th>状态</th>
            <th className="num">待上传</th>
            {!compact && <th className="num">失败</th>}
            {!compact && <th className="num">丢弃</th>}
            <th className="num">24h 事件</th>
            <th className="num">总事件</th>
            <th>最后在线</th>
            {!compact && <th>最后报告</th>}
            {!compact && <th>客户端</th>}
          </tr>
        </thead>
        <tbody>
          {devices.map((d) => {
            const st = syncState(d)
            return (
              <tr key={d.device_id} className={d.revoked_at ? 'dim' : ''}>
                <td>
                  <a href={href('sessions', { device_id: d.device_id })} className="strong">{d.name}</a>
                  <div className="muted small">{d.platform}</div>
                </td>
                <td>
                  <Badge tone={st.tone}>{st.label}</Badge>
                </td>
                <td className="num">
                  {d.spool_pending === undefined ? '—' : num(d.spool_pending)}
                  {d.spool_oldest_pending_at && (d.spool_pending ?? 0) > 0 && (
                    <div className="muted small">最早 {relTime(d.spool_oldest_pending_at)}</div>
                  )}
                </td>
                {!compact && <td className="num">{d.spool_dead === undefined ? '—' : num(d.spool_dead)}</td>}
                {!compact && <td className="num">{d.spool_dropped === undefined ? '—' : num(d.spool_dropped)}</td>}
                <td className="num">{num(d.events_24h)}</td>
                <td className="num">{num(d.events_total)}</td>
                <td>
                  <Time iso={d.last_seen_at} />
                </td>
                {!compact && (
                  <td>
                    <Time iso={d.spool_reported_at} />
                  </td>
                )}
                {!compact && (
                  <td>
                    <ClientVersion d={d} serverVersion={serverVersion} />
                  </td>
                )}
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

// The client version, marked when it is behind the server or its automatic
// upgrade failed.
function ClientVersion({ d, serverVersion }: { d: DeviceStatus; serverVersion?: string }) {
  const behind = !d.revoked_at && newerRelease(d.client_version, serverVersion)
  return (
    <>
      <span className="mono small">{d.client_version || '—'}</span>
      {behind && !d.upgrade_error && (
        <div>
          <Badge tone="warn">待升级 → {serverVersion}</Badge>
        </div>
      )}
      {behind && d.upgrade_error && (
        <div className="bad-text small" title={d.upgrade_error}>
          自动升级到 {d.upgrade_target || serverVersion} 失败：{d.upgrade_error}
        </div>
      )}
    </>
  )
}

const jobLabel: Record<string, string> = { queued: '排队', running: '运行中', done: '完成', failed: '失败' }

export function IndexStatus({ embedding, jobs }: { embedding: EmbeddingStatus; jobs: Record<string, number> }) {
  const sp = embedding.active_space
  const building = embedding.building_space
  return (
    <dl className="kv">
      <dt>语义搜索</dt>
      <dd>
        {!embedding.enabled ? (
          <span className="muted">未启用（仅关键词搜索）</span>
        ) : sp ? (
          <>
            <Badge tone="ok">可用</Badge> {sp.model} · {sp.dimensions} 维
          </>
        ) : (
          <Badge tone="warn">索引构建中</Badge>
        )}
      </dd>
      {sp && (
        <>
          <dt>已向量化</dt>
          <dd>
            {num(sp.docs_embedded)} 篇{sp.docs_pending > 0 && <span className="warn-text">，{num(sp.docs_pending)} 篇排队中</span>}
          </dd>
        </>
      )}
      {building && (
        <>
          <dt>新索引</dt>
          <dd>
            {building.model}：{num(building.docs_embedded)} / {num(building.docs_embedded + building.docs_pending)}
          </dd>
        </>
      )}
      {embedding.last_error && (
        <>
          <dt>最近错误</dt>
          <dd className="bad-text">
            {embedding.last_error} <span className="muted small">（{relTime(embedding.last_error_at)}）</span>
          </dd>
        </>
      )}
      {embedding.breaker_until && (
        <>
          <dt>熔断</dt>
          <dd className="warn-text">服务暂时跳过，{relTime(embedding.breaker_until)}恢复</dd>
        </>
      )}
      <dt>后台任务</dt>
      <dd>
        {Object.keys(jobs).length === 0 ? (
          <span className="muted">无</span>
        ) : (
          Object.entries(jobs).map(([k, v]) => (
            <span key={k} className="job-count">
              {jobLabel[k] ?? k} {num(v)}
            </span>
          ))
        )}
      </dd>
    </dl>
  )
}
