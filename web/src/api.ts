// API types mirror the Go server (internal/model, internal/server/handler).

export interface Device {
  device_id: string
  name: string
  platform: string
  created_at: string
  last_seen_at?: string
  revoked_at?: string
}

export interface DeviceStatus extends Device {
  events_total: number
  events_24h: number
  spool_pending?: number
  spool_dead?: number
  spool_dropped?: number
  spool_oldest_pending_at?: string
  spool_reported_at?: string
  client_version?: string
  upgrade_target?: string
  upgrade_error?: string
}

export interface Health {
  status: string
  version: string
  latest_version?: string
}

export interface Project {
  project_id: string
  name: string
  canonical_git_identity?: string
  created_at: string
  updated_at: string
}

export interface SessionInfo {
  session_id: string
  project_id: string
  project_name: string
  device_id: string
  device_name: string
  agent_type: string
  native_session_id: string
  status: string
  started_at: string
  ended_at?: string
  last_event_at?: string
  event_count: number
  objective?: string
  latest_request?: string
}

export interface TestResult { command: string; passed: boolean; exit_code?: number; at: string; event_id: string }
export interface Issue { title: string; error: string; at: string; event_id: string }
export interface Fix {
  title: string; error: string; files: string[]
  failed_event_id: string; fixed_event_id: string; file_event_ids: string[]; at: string
}

export interface Summary {
  status: string
  objective: string
  latest_request: string
  completed_work: string
  changed_files: string[]
  test_results: TestResult[]
  unresolved_issues: Issue[]
  next_steps: string[]
  evidence: string[]
  fixes?: Fix[]
  event_count: number
  first_event_at: string
  last_event_at: string
  generator: string
  generated_at: string
  disclaimer: string
}

export interface SessionDetail { session: SessionInfo; summary?: Summary }

export interface EventRecord {
  event_id: string
  event_type: string
  occurred_at: string
  payload: Record<string, unknown>
}

export interface EventPage { events: EventRecord[]; next?: string }
export interface EventDetail { event: EventRecord; session?: SessionInfo }

export interface SourceRef { kind: 'event' | 'session'; id: string }

export interface Memory {
  memory_id: string
  project_id: string
  project_name?: string
  session_id?: string
  scope: string
  type: string
  title: string
  content: string
  status: string
  confidence: number
  source_refs: SourceRef[]
  generator: string
  created_at: string
  updated_at: string
}

export interface SourceInfo extends SourceRef {
  event_type?: string
  occurred_at?: string
  summary?: string
  session_id?: string
  missing?: boolean
}

export interface MemoryDetail { memory: Memory; sources: SourceInfo[] }

export interface SpaceInfo { model: string; dimensions: number; fingerprint: string; docs_embedded: number; docs_pending: number }
export interface EmbeddingStatus {
  enabled: boolean
  url?: string
  service?: { model: string; dimensions: number }
  active_space?: SpaceInfo
  building_space?: SpaceInfo
  last_error?: string
  last_error_at?: string
  breaker_until?: string
}

export interface UnfinishedSession { session: SessionInfo; unresolved: number; next_steps: string[] }

export interface Overview {
  counts: { projects: number; devices: number; sessions_7d: number; events_24h: number; events_total: number; memories: number }
  devices: DeviceStatus[]
  recent_sessions: SessionInfo[]
  unfinished: UnfinishedSession[]
  latest_memories: Memory[]
  embedding: EmbeddingStatus
  jobs: Record<string, number>
}

export interface ProjectLocation {
  device_id: string; device_name: string; absolute_path: string
  git_branch?: string; git_head?: string; last_observed_at: string
}

export interface ProjectDetail {
  project: Project
  locations: ProjectLocation[]
  sessions: SessionInfo[]
  stats: { sessions: number; events: number; memories: number; last_activity_at?: string }
}

export interface SearchRequest {
  query: string
  scope?: string
  project_ids?: string[]
  kinds?: string[]
  agent_types?: string[]
  device_ids?: string[]
  since?: string
  limit?: number
  mode?: string
  highlight?: string
}

export interface SearchResult {
  doc_id: number
  kind: string
  ref_id: string
  project_id: string
  project_name: string
  session_id?: string
  agent_type?: string
  device_id?: string
  device_name?: string
  occurred_at: string
  title?: string
  snippet: string
  score: number
  match: string
  matched_terms: number
  total_terms: number
  similarity?: number
}

export interface SearchResponse { results: SearchResult[]; scope: string; note?: string }

export interface Invite {
  id: string
  note: string
  status: 'pending' | 'used' | 'expired' | 'revoked'
  created_at: string
  expires_at: string
  used_at?: string
  device_id?: string
  device_name?: string
}

export interface CreatedInvite { code: string; note: string; expires_at: string }
export interface LoginLink { code: string; path: string; expires_at: string }

export interface UISessionInfo {
  id: string
  role: 'admin' | 'viewer'
  user_agent: string
  device_name?: string
  created_at: string
  last_seen_at: string
  expires_at: string
  current: boolean
}

export interface ClientDownload { name: string; os: string; arch: string; size: number }

// A device asking to join without an invite (memspace onboard <server>).
export interface DeviceRequest {
  request_id: string
  user_code: string
  name: string
  platform: string
  remote_addr: string
  client_version: string
  status: 'pending' | 'approved'
  created_at: string
  expires_at: string
}

export class APIError extends Error {
  constructor(public status: number, message: string) {
    super(message)
  }
}

// Every request carries the UI header: the server rejects cookie-authenticated
// requests without it (a cross-site form or image cannot set headers).
async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { 'X-MemSpace-UI': '1' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const resp = await fetch(path, {
    method,
    headers,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!resp.ok) {
    let msg = resp.statusText
    try {
      const j = await resp.json()
      if (j && typeof j.error === 'string') msg = j.error
    } catch {
      /* not JSON */
    }
    if (resp.status === 401) window.dispatchEvent(new Event('memspace:unauthorized'))
    throw new APIError(resp.status, msg)
  }
  if (resp.status === 204) return undefined as T
  return (await resp.json()) as T
}

const q = (params: Record<string, string | number | undefined>) => {
  const s = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '') s.set(k, String(v))
  const str = s.toString()
  return str ? '?' + str : ''
}

export const api = {
  session: () => request<{ role: 'admin' | 'viewer' }>('GET', '/api/v1/ui/session'),
  health: () => request<Health>('GET', '/api/v1/health'),
  login: (code: string) => request<void>('POST', '/api/v1/ui/login', { code }),
  logout: () => request<void>('POST', '/api/v1/ui/logout'),
  overview: () => request<Overview>('GET', '/api/v1/overview'),
  projects: () => request<Project[]>('GET', '/api/v1/projects'),
  project: (id: string) => request<ProjectDetail>('GET', '/api/v1/projects/' + encodeURIComponent(id)),
  devices: () => request<Device[]>('GET', '/api/v1/devices'),
  sessions: (p: { project_id?: string; agent_type?: string; device_id?: string; status?: string; before?: string; before_id?: string; limit?: number }) =>
    request<SessionInfo[]>('GET', '/api/v1/sessions' + q(p)),
  sessionDetail: (id: string) => request<SessionDetail>('GET', '/api/v1/sessions/' + encodeURIComponent(id)),
  sessionEvents: (id: string, cursor?: string) =>
    request<EventPage>('GET', '/api/v1/sessions/' + encodeURIComponent(id) + '/events' + q({ cursor, limit: 200 })),
  event: (id: string) => request<EventDetail>('GET', '/api/v1/events/' + encodeURIComponent(id)),
  memories: (p: { project_id?: string; type?: string; status?: string; before?: string; before_id?: string; limit?: number }) =>
    request<Memory[]>('GET', '/api/v1/memories' + q(p)),
  memory: (id: string) => request<MemoryDetail>('GET', '/api/v1/memories/' + encodeURIComponent(id)),
  search: (req: SearchRequest) => request<SearchResponse>('POST', '/api/v1/search', { ...req, highlight: 'marks' }),
  embedding: () => request<EmbeddingStatus>('GET', '/api/v1/embedding/status'),
  admin: {
    invites: () => request<Invite[]>('GET', '/api/v1/admin/invites'),
    createInvite: (note: string, ttl_hours: number) => request<CreatedInvite>('POST', '/api/v1/admin/invites', { note, ttl_hours }),
    revokeInvite: (id: string) => request<void>('DELETE', '/api/v1/admin/invites/' + encodeURIComponent(id)),
    revokeDevice: (id: string) => request<void>('POST', '/api/v1/admin/devices/' + encodeURIComponent(id) + '/revoke'),
    loginLink: (role: string) => request<LoginLink>('POST', '/api/v1/admin/login-links', { role }),
    sessions: () => request<UISessionInfo[]>('GET', '/api/v1/admin/ui-sessions'),
    revokeSession: (id: string) => request<void>('DELETE', '/api/v1/admin/ui-sessions/' + encodeURIComponent(id)),
    downloads: () => request<ClientDownload[]>('GET', '/api/v1/admin/downloads'),
    deviceRequests: () => request<{ requests: DeviceRequest[] }>('GET', '/api/v1/admin/device-requests').then((r) => r.requests),
    approveDevice: (id: string) => request<void>('POST', '/api/v1/admin/device-requests/' + encodeURIComponent(id) + '/approve'),
    denyDevice: (id: string) => request<void>('POST', '/api/v1/admin/device-requests/' + encodeURIComponent(id) + '/deny'),
  },
}
