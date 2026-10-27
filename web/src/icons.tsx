// Minimal stroke icons (24×24, lucide-style), inlined to keep the bundle
// dependency-free.
const paths: Record<string, string[]> = {
  overview: ['M3 3h7v9H3z', 'M14 3h7v5h-7z', 'M14 12h7v9h-7z', 'M3 16h7v5H3z'],
  projects: ['M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z'],
  sessions: ['M4 6h16', 'M4 12h16', 'M4 18h10'],
  search: ['M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14z', 'M21 21l-4.3-4.3'],
  memories: ['M12 3l8 4.5v9L12 21l-8-4.5v-9z', 'M12 12l8-4.5', 'M12 12v9', 'M12 12L4 7.5'],
  devices: ['M3 5h18v11H3z', 'M8 20h8', 'M12 16v4'],
  logout: ['M15 4h4v16h-4', 'M10 16l4-4-4-4', 'M14 12H4'],
  refresh: ['M20 11a8 8 0 1 0-2.3 5.7', 'M20 4v7h-7'],
  admin: ['M12 3l7 3v5c0 4.5-3 8.3-7 10-4-1.7-7-5.5-7-10V6z', 'M9 12l2 2 4-4'],
  copy: ['M9 9h11v11H9z', 'M5 15H4V4h11v1'],
  check: ['M5 12l5 5 9-10'],
  plus: ['M12 5v14', 'M5 12h14'],
}

export function Icon({ name, size = 16 }: { name: keyof typeof paths | string; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.8}
      strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" className="icon">
      {(paths[name] ?? []).map((d, i) => <path key={i} d={d} />)}
    </svg>
  )
}

export function Logo() {
  return (
    <svg width="22" height="22" viewBox="0 0 24 24" aria-hidden="true" className="logo">
      <circle cx="6" cy="7" r="2.6" fill="currentColor" opacity=".55" />
      <circle cx="18" cy="7" r="2.6" fill="currentColor" opacity=".55" />
      <circle cx="12" cy="17" r="3.2" fill="currentColor" />
      <path d="M7.8 8.6l2.8 5.6M16.2 8.6l-2.8 5.6M8.6 7h6.8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" opacity=".7" />
    </svg>
  )
}
