import { useEffect, useRef, useState, type FormEvent } from 'react'
import { api, APIError } from './api'
import { Loading } from './components'
import { Icon, Logo } from './icons'
import { href, navigate, useRoute, type Route } from './router'
import { Admin } from './pages/Admin'
import { Devices } from './pages/Devices'
import { Memories, MemoryPage } from './pages/Memories'
import { Overview } from './pages/Overview'
import { ProjectPage, Projects } from './pages/Projects'
import { Search } from './pages/Search'
import { EventPage, SessionPage, Sessions } from './pages/Sessions'

type Auth = 'checking' | 'in' | 'out'

export function App() {
  const route = useRoute()
  const [auth, setAuth] = useState<Auth>('checking')
  const [role, setRole] = useState<'admin' | 'viewer'>('viewer')
  const [loginError, setLoginError] = useState('')
  const redeemed = useRef('')

  useEffect(() => {
    const out = () => setAuth('out')
    window.addEventListener('memspace:unauthorized', out)
    return () => window.removeEventListener('memspace:unauthorized', out)
  }, [])

  // A login link (#/login/<code>) is redeemed once, then removed from the
  // address bar and history.
  const code = route.path[0] === 'login' ? route.path[1] ?? '' : ''
  useEffect(() => {
    if (code) {
      if (redeemed.current === code) return
      redeemed.current = code
      window.history.replaceState(null, '', '#/login')
      api.login(code).then(
        () => {
          setLoginError('')
          setAuth('checking')
          navigate('', undefined, true)
        },
        (e: Error) => {
          setLoginError(e instanceof APIError && e.status === 401 ? '登录链接无效、已过期或已被使用。' : e.message)
          setAuth('out')
        },
      )
      return
    }
    if (auth === 'checking') {
      api.session().then(
        (s) => {
          setRole(s.role)
          setAuth('in')
        },
        (e: Error) => {
          if (e instanceof APIError && (e.status === 401 || e.status === 403)) setAuth('out')
          else setLoginError(e.message)
        },
      )
    }
  }, [code, auth])

  if (auth === 'checking') return loginError ? <LoginScreen error={loginError} /> : <div className="center"><Loading /></div>
  if (auth === 'out') return <LoginScreen error={loginError} />
  return (
    <Shell route={route} admin={role === 'admin'} onLogout={() => api.logout().finally(() => setAuth('out'))}>
      <Page route={route} admin={role === 'admin'} />
    </Shell>
  )
}

function LoginScreen({ error }: { error?: string }) {
  return (
    <div className="login">
      <div className="login-card">
        <div className="brand big">
          <Logo />
          <span>MemSpace</span>
        </div>
        <p>需要登录才能查看记忆与历史。</p>
        {error && <div className="error-box">{error}</div>}
        <p>在任意已连接的设备上运行：</p>
        <pre className="cmd">memspace ui</pre>
        <p className="muted small">
          它会生成一次性登录链接（5 分钟内有效）并打开浏览器。也可以在服务器上运行 <code>memspace server login-link</code>。
        </p>
      </div>
    </div>
  )
}

const nav: [string, string][] = [
  ['', '概览'],
  ['projects', '项目'],
  ['sessions', '会话'],
  ['search', '搜索'],
  ['memories', '记忆'],
  ['devices', '设备与同步'],
]
const navIcon: Record<string, string> = { '': 'overview', projects: 'projects', sessions: 'sessions', search: 'search', memories: 'memories', devices: 'devices', admin: 'admin' }

function Shell({ route, admin, onLogout, children }: { route: Route; admin: boolean; onLogout: () => void; children: React.ReactNode }) {
  const section = route.path[0] ?? ''
  const [q, setQ] = useState('')
  const submit = (e: FormEvent) => {
    e.preventDefault()
    if (q.trim()) navigate('search', { q: q.trim() })
    setQ('')
  }
  const current = section === 'events' ? 'sessions' : section === 'login' ? '' : section
  return (
    <div className="shell">
      <header className="topbar">
        <a className="brand" href={href('')}>
          <Logo />
          <span>MemSpace</span>
        </a>
        {section !== 'search' && (
          <form className="quick-search" onSubmit={submit} role="search">
            <Icon name="search" size={15} />
            <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder="搜索所有设备的历史…" aria-label="搜索" />
          </form>
        )}
        <span className={'role-chip' + (admin ? ' admin' : '')} title={admin ? '管理员会话' : '只读会话'}>{admin ? '管理员' : '只读'}</span>
        <button className="btn-ghost logout" onClick={onLogout} title="退出登录">
          <Icon name="logout" size={15} />
          <span>退出</span>
        </button>
      </header>
      <div className="body">
        <nav className="sidebar">
          {nav.map(([p, label]) => (
            <a key={p} href={href(p)} className={current === p ? 'active' : ''}>
              <Icon name={navIcon[p]} />
              <span>{label}</span>
            </a>
          ))}
          {admin && (
            <>
              <div className="nav-sep" />
              <a href={href('admin')} className={current === 'admin' ? 'active' : ''}>
                <Icon name="admin" />
                <span>管理</span>
              </a>
            </>
          )}
        </nav>
        <main>{children}</main>
      </div>
    </div>
  )
}

function Page({ route, admin }: { route: Route; admin: boolean }) {
  const [section, id] = route.path
  switch (section ?? '') {
    case '':
    case 'login':
      return <Overview />
    case 'projects':
      return id ? <ProjectPage id={id} /> : <Projects />
    case 'sessions':
      return id ? <SessionPage id={id} route={route} /> : <Sessions route={route} />
    case 'events':
      return <EventPage id={id ?? ''} />
    case 'search':
      return <Search route={route} />
    case 'memories':
      return id ? <MemoryPage id={id} /> : <Memories route={route} />
    case 'devices':
      return <Devices />
    case 'admin':
      return admin ? <Admin /> : (
        <div className="page">
          <h1>需要管理员权限</h1>
          <p className="hint">当前是只读会话（来自 <code>memspace ui</code>）。在服务器上运行 <code>memspace server login-link</code> 获取管理员登录链接。</p>
        </div>
      )
    default:
      return <div className="page"><h1>页面不存在</h1><a href={href('')}>回到概览</a></div>
  }
}
