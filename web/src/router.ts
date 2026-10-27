import { useEffect, useState } from 'react'

// Hash routing: the server only serves "/", and login codes in the fragment
// never reach server logs.

export interface Route {
  path: string[] // e.g. ['sessions', 'ses_123']
  params: URLSearchParams
}

export function parseHash(hash: string): Route {
  const h = hash.replace(/^#\/?/, '')
  const [p, qs = ''] = h.split('?', 2)
  return { path: p.split('/').filter(Boolean).map(decodeURIComponent), params: new URLSearchParams(qs) }
}

export function useRoute(): Route {
  const [route, setRoute] = useState(() => parseHash(window.location.hash))
  useEffect(() => {
    const on = () => setRoute(parseHash(window.location.hash))
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  return route
}

export function href(path: string, params?: Record<string, string | undefined>): string {
  let s = '#/' + path.replace(/^\//, '')
  if (params) {
    const q = new URLSearchParams()
    for (const [k, v] of Object.entries(params)) if (v) q.set(k, v)
    const str = q.toString()
    if (str) s += '?' + str
  }
  return s
}

export function navigate(path: string, params?: Record<string, string | undefined>, replace = false) {
  const h = href(path, params)
  if (replace) window.history.replaceState(null, '', h)
  else window.history.pushState(null, '', h)
  window.dispatchEvent(new HashChangeEvent('hashchange'))
}
