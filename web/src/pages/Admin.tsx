import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { api, type ClientDownload, type CreatedInvite, type Device, type DeviceRequest, type Invite, type LoginLink, type UISessionInfo } from '../api'
import { Async, Badge, Card, Empty, ErrorBox, Time, useAsync } from '../components'
import { Icon } from '../icons'
import { absTime, isRelease } from '../util'

// copyText works on plain-HTTP LAN servers too, where navigator.clipboard
// (secure contexts only) is unavailable.
async function copyText(text: string): Promise<boolean> {
  try {
    if (window.isSecureContext && navigator.clipboard) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* fall back */
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.className = 'offscreen'
  document.body.appendChild(ta)
  ta.select()
  let ok = false
  try {
    ok = document.execCommand('copy')
  } catch {
    ok = false
  }
  ta.remove()
  return ok
}

export function CopyBlock({ text, label }: { text: string; label?: string }) {
  const [state, setState] = useState<'' | 'ok' | 'fail'>('')
  const copy = async () => {
    setState((await copyText(text)) ? 'ok' : 'fail')
    setTimeout(() => setState(''), 1800)
  }
  return (
    <div className="copy-block">
      {label && <div className="event-block-label">{label}</div>}
      <div className="copy-row">
        <pre className="cmd-block">{text}</pre>
        <button className="btn-ghost copy-btn" onClick={copy} title="复制">
          <Icon name={state === 'ok' ? 'check' : 'copy'} size={14} />
          <span>{state === 'ok' ? '已复制' : state === 'fail' ? '请手动复制' : '复制'}</span>
        </button>
      </div>
    </div>
  )
}

// Confirm is an inline two-step button (the viewer has no confirm dialogs).
function Confirm({ label, confirm, onConfirm, disabled }: { label: string; confirm: string; onConfirm: () => Promise<void>; disabled?: boolean }) {
  const [asking, setAsking] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  if (disabled) return null
  if (!asking)
    return (
      <button className="btn-danger-ghost" onClick={() => setAsking(true)}>
        {label}
      </button>
    )
  return (
    <span className="confirm">
      <span className="small">{error || confirm}</span>
      <button className="btn-danger" disabled={busy}
        onClick={async () => {
          setBusy(true)
          try {
            await onConfirm()
            setAsking(false)
          } catch (e) {
            setError((e as Error).message)
          } finally {
            setBusy(false)
          }
        }}>
        确认
      </button>
      <button className="btn-ghost" onClick={() => { setAsking(false); setError('') }} disabled={busy}>
        取消
      </button>
    </span>
  )
}

const platforms: { key: string; label: string; os: string; arch: string }[] = [
  { key: 'darwin-arm64', label: 'macOS · Apple 芯片', os: 'darwin', arch: 'arm64' },
  { key: 'darwin-amd64', label: 'macOS · Intel', os: 'darwin', arch: 'amd64' },
  { key: 'linux-amd64', label: 'Linux · x86_64', os: 'linux', arch: 'amd64' },
  { key: 'linux-arm64', label: 'Linux · ARM64', os: 'linux', arch: 'arm64' },
  { key: 'windows-amd64', label: 'Windows', os: 'windows', arch: 'amd64' },
]

function shellQuote(s: string) {
  return /^[A-Za-z0-9._-]+$/.test(s) ? s : "'" + s.replace(/'/g, `'\\''`) + "'"
}

// Instructions an agent follows to set up a device (docs/agent-setup.md).
const agentSetupURL = 'https://raw.githubusercontent.com/chux0519/memspace/main/docs/agent-setup.md'

function agentPrompt(invite: CreatedInvite) {
  const name = invite.note.trim()
  return (
    `帮我在这台机器上安装 MemSpace：按 ${agentSetupURL} 的步骤操作。` +
    `服务器 ${window.location.origin}，邀请码 ${invite.code}` +
    (name ? `，设备名 ${name}` : '') +
    '。'
  )
}

// Release builds are installed from GitHub with the install scripts; the
// version matches this server's when it is a release.
const installScripts = 'https://raw.githubusercontent.com/chux0519/memspace/main'

function setupCommands(p: (typeof platforms)[number], invite: CreatedInvite, downloads: ClientDownload[], version?: string) {
  const origin = window.location.origin
  const name = invite.note.trim()
  const file = `memspace-${p.os}-${p.arch}${p.os === 'windows' ? '.exe' : ''}`
  // Served by this server (a mirror for networks without GitHub access).
  const mirrored = downloads.some((d) => d.name === file)
  const pin = isRelease(version) ? version : ''
  if (p.os === 'windows') {
    const install = `${pin ? `$env:MEMSPACE_VERSION = "${pin}"; ` : ''}irm ${installScripts}/install.ps1 | iex`
    const mirror = `New-Item -ItemType Directory -Force "$HOME\\bin" | Out-Null\nInvoke-WebRequest ${origin}/downloads/${file} -OutFile "$HOME\\bin\\memspace.exe"`
    const onboard = `& "$HOME\\bin\\memspace.exe" onboard ${origin} --invite ${invite.code}${name ? ` --name "${name.replace(/"/g, '')}"` : ''}`
    return { install, mirror: mirrored ? mirror : '', onboard }
  }
  const install = `curl -fsSL ${installScripts}/install.sh | sh${pin ? ` -s -- --version ${pin}` : ''}`
  const mirror = `mkdir -p ~/.local/bin\ncurl -fsSL --noproxy '*' ${origin}/downloads/${file} -o ~/.local/bin/memspace\nchmod +x ~/.local/bin/memspace`
  const onboard = `~/.local/bin/memspace onboard ${origin} --invite ${invite.code}${name ? ' --name ' + shellQuote(name) : ''}`
  return { install, mirror: mirrored ? mirror : '', onboard }
}

function AddDevice({ downloads, version, onCreated }: { downloads: ClientDownload[]; version?: string; onCreated: () => void }) {
  const [note, setNote] = useState('')
  const [ttl, setTTL] = useState('24')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error>()
  const [invite, setInvite] = useState<CreatedInvite>()
  const [platform, setPlatform] = useState(() => {
    const ua = navigator.userAgent
    return /Windows/.test(ua) ? 'windows-amd64' : /Mac/.test(ua) ? 'darwin-arm64' : 'linux-amd64'
  })
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      setInvite(await api.admin.createInvite(note, Number(ttl)))
      onCreated()
    } catch (err) {
      setError(err as Error)
    } finally {
      setBusy(false)
    }
  }
  const p = platforms.find((x) => x.key === platform)!
  const cmds = invite ? setupCommands(p, invite, downloads, version) : undefined
  return (
    <Card title="添加设备" actions={<span className="muted small">每台设备一个邀请码，只能使用一次</span>}>
      <form className="inline-form" onSubmit={submit}>
        <label className="field grow">
          <span>设备名称</span>
          <input value={note} onChange={(e) => setNote(e.target.value)} placeholder="例如 macmini、windows-dev（可选）" maxLength={80} />
        </label>
        <label className="field">
          <span>有效期</span>
          <select value={ttl} onChange={(e) => setTTL(e.target.value)}>
            <option value="1">1 小时</option>
            <option value="24">24 小时</option>
            <option value="168">7 天</option>
          </select>
        </label>
        <button className="btn" type="submit" disabled={busy}>
          <Icon name="plus" size={15} />
          生成邀请码
        </button>
      </form>
      {error && <ErrorBox error={error} />}
      {invite && cmds && (
        <div className="setup">
          <div className="setup-head">
            <div>
              <div className="event-block-label">邀请码</div>
              <code className="invite-code">{invite.code}</code>
              <div className="muted small">只显示这一次 · {absTime(invite.expires_at)} 前有效</div>
            </div>
          </div>
          <CopyBlock label="让 Agent 安装：复制给这台机器上的 Claude Code、Codex 或 DSH" text={agentPrompt(invite)} />
          <p className="hint">Agent 会自己识别平台、下载 client、运行 onboard 并自检；修改它的配置前会征求你的同意。也可以按下面的命令手动安装。</p>
          <div className="tabs" role="tablist">
            {platforms.map((x) => (
              <button key={x.key} role="tab" aria-selected={x.key === platform} className={'tab' + (x.key === platform ? ' on' : '')} onClick={() => setPlatform(x.key)}>
                {x.label}
              </button>
            ))}
          </div>
          <CopyBlock label={'1. 安装 memspace（从 GitHub 下载并校验）' + (p.os === 'windows' ? '，PowerShell' : '')} text={cmds.install} />
          {cmds.mirror && <CopyBlock label="或者，这台机器访问不了 GitHub 时：从本服务器下载" text={cmds.mirror} />}
          <CopyBlock label="2. 连接并安装 Claude Code / Codex / DSH 采集" text={cmds.onboard} />
          <p className="hint">onboard 会注册设备、为本机找到的 agent 安装采集 hook 和 MCP，并运行自检。之后在那台机器上运行 <code>memspace ui</code> 即可打开已登录的界面。</p>
        </div>
      )}
    </Card>
  )
}

const inviteTone: Record<Invite['status'], [string, string]> = {
  pending: ['live', '待使用'],
  used: ['ok', '已使用'],
  expired: ['neutral', '已过期'],
  revoked: ['bad', '已撤销'],
}

function Invites({ invites, reload }: { invites: Invite[]; reload: () => void }) {
  if (invites.length === 0) return <Empty>还没有邀请码。</Empty>
  return (
    <div className="table-wrap">
      <table className="table">
        <thead>
          <tr>
            <th>设备名称</th>
            <th>状态</th>
            <th>注册的设备</th>
            <th>创建</th>
            <th>过期</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {invites.map((i) => (
            <tr key={i.id} className={i.status === 'expired' || i.status === 'revoked' ? 'dim' : ''}>
              <td className="strong">{i.note || <span className="muted">—</span>}</td>
              <td><Badge tone={inviteTone[i.status][0]}>{inviteTone[i.status][1]}</Badge></td>
              <td>{i.device_name ? <>{i.device_name} <span className="muted small"><Time iso={i.used_at} /></span></> : '—'}</td>
              <td><Time iso={i.created_at} /></td>
              <td>{absTime(i.expires_at)}</td>
              <td className="actions">
                <Confirm label="撤销" confirm="撤销这个邀请码？" disabled={i.status !== 'pending'}
                  onConfirm={async () => { await api.admin.revokeInvite(i.id); reload() }} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// Devices that ran `memspace onboard <server>` without an invite. Polled,
// so a request shows up while the user is looking at this page.
function PendingDevices({ onChange }: { onChange: () => void }) {
  const reqs = useAsync(api.admin.deviceRequests, [])
  const { reload } = reqs
  useEffect(() => {
    const t = setInterval(reload, 3000)
    return () => clearInterval(t)
  }, [reload])
  // A claimed request disappears and its device appears: refresh devices.
  const count = reqs.data?.length
  const prev = useRef(count)
  useEffect(() => {
    if (prev.current !== undefined && count !== undefined && count < prev.current) onChange()
    prev.current = count
  }, [count, onChange])
  const list = reqs.data ?? []
  return (
    <Card title="待审批设备" className={list.some((r) => r.status === 'pending') ? 'card-attention' : ''}
      actions={<span className="muted small">核对码与设备上显示的一致再批准</span>}>
      {reqs.error && <ErrorBox error={reqs.error} />}
      {list.length === 0 ? (
        <>
          <Empty>没有等待批准的设备。</Empty>
          <div className="setup">
            <CopyBlock label="添加设备：把这句话发给新设备上的 Claude Code、Codex 或 DSH" text={`帮我在这台机器上安装 MemSpace：按 ${agentSetupURL} 的步骤操作。服务器 ${window.location.origin}。`} />
            <p className="hint">Agent 会安装 client 并显示一个核对码，它会出现在这里等你批准。也可以在新设备上自己运行 <code>memspace onboard {window.location.origin}</code>。</p>
          </div>
        </>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>核对码</th>
                <th>设备</th>
                <th>来源</th>
                <th>申请时间</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.map((r) => (
                <PendingRow key={r.request_id} r={r} reload={reload} />
              ))}
            </tbody>
          </table>
        </div>
      )}
      {list.some((r) => r.status === 'pending') && (
        <p className="hint">批准后设备会自动完成连接（Agent 安装时，告诉它已批准即可）。</p>
      )}
    </Card>
  )
}

function PendingRow({ r, reload }: { r: DeviceRequest; reload: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const approve = async () => {
    setBusy(true)
    setError('')
    try {
      await api.admin.approveDevice(r.request_id)
      reload()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <tr>
      <td><code className="invite-code">{r.user_code}</code></td>
      <td>
        <div className="strong">{r.name}</div>
        <div className="muted small">{r.platform}{r.client_version ? ' · ' + r.client_version : ''}</div>
      </td>
      <td className="mono">{r.remote_addr}</td>
      <td><Time iso={r.created_at} /></td>
      <td className="actions">
        {r.status === 'approved' ? (
          <Badge tone="live">已批准，等待设备连接</Badge>
        ) : (
          <>
            {error && <span className="small bad-text">{error}</span>}
            <button className="btn" disabled={busy} onClick={approve}>
              <Icon name="check" size={15} />
              批准
            </button>
            <Confirm label="拒绝" confirm="拒绝后这台设备需要重新申请。"
              onConfirm={async () => { await api.admin.denyDevice(r.request_id); reload() }} />
          </>
        )}
      </td>
    </tr>
  )
}

function Devices({ devices, reload }: { devices: Device[]; reload: () => void }) {
  if (devices.length === 0) return <Empty>还没有设备。</Empty>
  return (
    <div className="table-wrap">
      <table className="table">
        <thead>
          <tr>
            <th>设备</th>
            <th>平台</th>
            <th>注册</th>
            <th>最后在线</th>
            <th>状态</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {devices.map((d) => (
            <tr key={d.device_id} className={d.revoked_at ? 'dim' : ''}>
              <td>
                <div className="strong">{d.name}</div>
                <div className="mono muted small">{d.device_id}</div>
              </td>
              <td>{d.platform}</td>
              <td>{absTime(d.created_at)}</td>
              <td><Time iso={d.last_seen_at} /></td>
              <td>{d.revoked_at ? <Badge tone="bad">已撤销</Badge> : <Badge tone="ok">正常</Badge>}</td>
              <td className="actions">
                <Confirm label="撤销" confirm="撤销后它的令牌立即失效，已采集的数据保留。" disabled={!!d.revoked_at}
                  onConfirm={async () => { await api.admin.revokeDevice(d.device_id); reload() }} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function browserName(ua: string) {
  const b = /Edg\//.test(ua) ? 'Edge' : /Chrome\//.test(ua) ? 'Chrome' : /Firefox\//.test(ua) ? 'Firefox' : /Safari\//.test(ua) ? 'Safari' : ''
  const os = /Windows/.test(ua) ? 'Windows' : /iPhone|iPad/.test(ua) ? 'iOS' : /Android/.test(ua) ? 'Android' : /Mac OS X/.test(ua) ? 'macOS' : /Linux/.test(ua) ? 'Linux' : ''
  return [b, os].filter(Boolean).join(' · ') || ua.slice(0, 40) || '未知浏览器'
}

function Sessions({ sessions, reload }: { sessions: UISessionInfo[]; reload: () => void }) {
  return (
    <div className="table-wrap">
      <table className="table">
        <thead>
          <tr>
            <th>浏览器</th>
            <th>权限</th>
            <th>来源</th>
            <th>登录</th>
            <th>最近活动</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {sessions.map((s) => (
            <tr key={s.id}>
              <td>
                <span className="strong">{browserName(s.user_agent)}</span> {s.current && <Badge tone="live">当前</Badge>}
              </td>
              <td><Badge tone={s.role === 'admin' ? 'file' : 'neutral'}>{s.role === 'admin' ? '管理员' : '只读'}</Badge></td>
              <td>{s.device_name ? `设备 ${s.device_name}` : '服务器登录链接'}</td>
              <td>{absTime(s.created_at)}</td>
              <td><Time iso={s.last_seen_at} /></td>
              <td className="actions">
                <Confirm label="踢出" confirm="让这个浏览器退出登录？" disabled={s.current}
                  onConfirm={async () => { await api.admin.revokeSession(s.id); reload() }} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function NewLoginLink() {
  const [role, setRole] = useState('viewer')
  const [link, setLink] = useState<LoginLink>()
  const [error, setError] = useState<Error>()
  const make = async () => {
    setError(undefined)
    try {
      setLink(await api.admin.loginLink(role))
    } catch (e) {
      setError(e as Error)
    }
  }
  return (
    <div className="login-link">
      <div className="inline-form">
        <label className="field">
          <span>权限</span>
          <select value={role} onChange={(e) => { setRole(e.target.value); setLink(undefined) }}>
            <option value="viewer">只读</option>
            <option value="admin">管理员</option>
          </select>
        </label>
        <button className="btn-ghost" onClick={make}>为另一个浏览器生成登录链接</button>
      </div>
      {error && <ErrorBox error={error} />}
      {link && <CopyBlock label={`登录链接 · 一次性 · ${absTime(link.expires_at)} 前有效`} text={window.location.origin + link.path} />}
    </div>
  )
}

function Section({ title, children, actions }: { title: string; children: ReactNode; actions?: ReactNode }) {
  return (
    <Card title={title} actions={actions}>
      {children}
    </Card>
  )
}

export function Admin() {
  const invites = useAsync(api.admin.invites, [])
  const devices = useAsync(api.devices, [])
  const sessions = useAsync(api.admin.sessions, [])
  const downloads = useAsync(api.admin.downloads, [])
  const health = useAsync(api.health, [])
  return (
    <div className="page">
      <header className="page-head">
        <div>
          <h1>管理</h1>
          <div className="muted">批准新设备、撤销访问、管理浏览器登录。</div>
        </div>
      </header>
      <PendingDevices onChange={devices.reload} />
      <AddDevice downloads={downloads.data ?? []} version={health.data?.version} onCreated={invites.reload} />
      <Section title="邀请码">
        <Async state={invites}>{(list) => <Invites invites={list} reload={invites.reload} />}</Async>
      </Section>
      <Section title="设备">
        <Async state={devices}>{(list) => <Devices devices={list} reload={devices.reload} />}</Async>
      </Section>
      <Section title="浏览器登录">
        <Async state={sessions}>{(list) => <Sessions sessions={list} reload={sessions.reload} />}</Async>
        <NewLoginLink />
      </Section>
    </div>
  )
}
