import type { EventRecord } from './api'

const rtf = new Intl.RelativeTimeFormat('zh-CN', { numeric: 'auto' })

export function relTime(iso?: string): string {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  if (!isFinite(t)) return '—'
  const s = (t - Date.now()) / 1000
  const a = Math.abs(s)
  if (a < 45) return '刚刚'
  if (a < 3600) return rtf.format(Math.round(s / 60), 'minute')
  if (a < 86400) return rtf.format(Math.round(s / 3600), 'hour')
  if (a < 86400 * 30) return rtf.format(Math.round(s / 86400), 'day')
  return absTime(iso)
}

export function absTime(iso?: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (!isFinite(d.getTime())) return '—'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export function clockTime(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

export function dayKey(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleDateString('zh-CN', { year: 'numeric', month: 'long', day: 'numeric', weekday: 'short' })
}

export function duration(from?: string, to?: string): string {
  if (!from || !to) return ''
  const s = Math.max(0, (new Date(to).getTime() - new Date(from).getTime()) / 1000)
  if (s < 60) return `${Math.round(s)} 秒`
  if (s < 3600) return `${Math.round(s / 60)} 分钟`
  const h = Math.floor(s / 3600)
  const m = Math.round((s % 3600) / 60)
  return m ? `${h} 小时 ${m} 分` : `${h} 小时`
}

export function num(n?: number): string {
  return (n ?? 0).toLocaleString('zh-CN')
}

export function shortId(id: string): string {
  const i = id.indexOf('_')
  const body = i >= 0 ? id.slice(i + 1) : id
  return body.slice(0, 8)
}

export function oneLine(s: string, max = 160): string {
  const t = s.replace(/\s+/g, ' ').trim()
  return t.length > max ? t.slice(0, max) + '…' : t
}

export const agentLabel: Record<string, string> = { 'claude-code': 'Claude Code', codex: 'Codex' }

export const eventLabel: Record<string, string> = {
  'session.started': '会话开始',
  'session.ended': '会话结束',
  'user.prompt': '用户请求',
  'agent.turn_completed': '回复',
  'tool.started': '工具调用',
  'tool.completed': '工具完成',
  'tool.failed': '工具失败',
  'file.modified': '文件修改',
  'context.compacted': '上下文压缩',
  'session.compacted': '上下文压缩',
  'agent.message': '进展',
  'agent.turn_aborted': '中止',
}

export const kindLabel: Record<string, string> = {
  event: '事件',
  prompt: '用户请求',
  reply: '回复',
  failure: '失败',
  command: '命令',
  change: '文件修改',
  summary: '会话摘要',
  memory: '记忆',
}

export const memoryTypeLabel: Record<string, string> = {
  episodic: '情景',
  procedural: '流程 / 修复',
  semantic: '事实',
  preference: '偏好',
}

export const statusLabel: Record<string, string> = {
  active: '进行中',
  ended: '已结束',
  interrupted: '中断',
  candidate: '候选',
  verified: '已确认',
  retracted: '已撤回',
}

type Payload = Record<string, unknown>

const str = (v: unknown): string => (typeof v === 'string' ? v : '')

function toolInput(p: Payload): Payload {
  const ti = p.tool_input
  return ti && typeof ti === 'object' ? (ti as Payload) : {}
}

// headline renders the gist of an event in one line.
export function headline(e: EventRecord): string {
  const p = e.payload ?? {}
  const ti = toolInput(p)
  switch (e.event_type) {
    case 'user.prompt':
      return str(p.prompt)
    case 'agent.turn_completed':
      return str(p.last_assistant_message)
    case 'agent.message':
      return str(p.message) || str(p.text)
    case 'file.modified': {
      const files = Array.isArray(p.files) ? (p.files as Payload[]).map((f) => str(f.path)).filter(Boolean) : []
      return str(p.path) || str(ti.file_path) || files.join(', ')
    }
    case 'session.started':
      return [str(p.source), str(p.cwd)].filter(Boolean).join(' · ')
    case 'session.ended':
      return str(p.reason)
    default: {
      const tool = str(p.tool_name)
      const detail = str(ti.command) || str(ti.file_path) || str(ti.pattern) || str(ti.query) || str(ti.url) || str(ti.description)
      return [tool, detail].filter(Boolean).join(' · ') || str(p.error)
    }
  }
}

export function eventTone(type: string): string {
  if (type === 'tool.failed') return 'bad'
  if (type === 'user.prompt') return 'user'
  if (type === 'agent.turn_completed' || type === 'agent.message') return 'agent'
  if (type === 'file.modified') return 'file'
  if (type.startsWith('session.')) return 'meta'
  return 'tool'
}

// pageCursor returns the before/before_id cursor for the next page after
// the last item of a session list.
export function sessionCursor(last: { last_event_at?: string; started_at: string; session_id: string }) {
  return { before: last.last_event_at ?? last.started_at, before_id: last.session_id }
}
