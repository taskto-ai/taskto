// A small router on the History API: the server answers every path outside /api with the
// app (internal/webui), so a URL can be bookmarked and reloaded.

import { type AnchorHTMLAttributes, type MouseEvent, useSyncExternalStore } from 'react'

const listeners = new Set<() => void>()

function subscribe(listener: () => void) {
  listeners.add(listener)
  window.addEventListener('popstate', listener)
  return () => {
    listeners.delete(listener)
    window.removeEventListener('popstate', listener)
  }
}

export function navigate(path: string, { replace = false } = {}) {
  if (path === window.location.pathname) return
  if (replace) window.history.replaceState(null, '', path)
  else window.history.pushState(null, '', path)
  for (const listener of listeners) listener()
}

export function usePath(): string {
  return useSyncExternalStore(subscribe, () => window.location.pathname)
}

export type Section = 'home' | 'conversations' | 'knowledge' | 'boards' | 'apps' | 'settings'
export const SECTIONS: readonly Section[] = ['home', 'conversations', 'knowledge', 'boards', 'apps', 'settings']

// /w/{workspace}/{section}[/{item}]: anything else is null, and the shell picks a default.
export type Route = { workspace: number; section: Section; item?: string }

export function parseRoute(path: string): Route | null {
  const m = /^\/w\/(\d+)\/([a-z]+)(?:\/([^/]+))?\/?$/.exec(path)
  if (!m) return null
  const section = m[2] as Section
  if (!SECTIONS.includes(section)) return null
  let item: string | undefined
  if (m[3]) {
    try {
      item = decodeURIComponent(m[3])
    } catch {
      return null // a malformed %-sequence, typed or pasted: not a route
    }
  }
  return { workspace: Number(m[1]), section, ...(item ? { item } : {}) }
}

// Where a link to a place in the boards or the knowledge base goes: the item says which, as the page
// that opens it reads it back (boardItem, documentItem).
export function boardItem(space: number, board?: number, card?: number): string {
  return [space, board, card].filter((n) => n !== undefined).join(':')
}

export function parseBoardItem(item: string | undefined): { space: number; board?: number; card?: number } | null {
  const m = /^(\d+)(?::(\d+))?(?::(\d+))?$/.exec(item ?? '')
  if (!m) return null
  return { space: Number(m[1]), ...(m[2] ? { board: Number(m[2]) } : {}), ...(m[3] ? { card: Number(m[3]) } : {}) }
}

export function documentItem(space: number, path: string): string {
  return `${space}:${path}`
}

export function parseDocumentItem(item: string | undefined): { space: number; path?: string } | null {
  const m = /^(\d+)(?::(.+))?$/.exec(item ?? '')
  return m ? { space: Number(m[1]), ...(m[2] ? { path: m[2] } : {}) } : null
}

export function routePath(r: Route): string {
  return `/w/${r.workspace}/${r.section}${r.item ? `/${encodeURIComponent(r.item)}` : ''}`
}

// An <a> that navigates in the app, and leaves modified clicks (new tab, ...) to the browser.
export function Link({ href, onClick, ...rest }: AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) {
  const click = (e: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(e)
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
    e.preventDefault()
    navigate(href)
  }
  return <a href={href} onClick={click} {...rest} />
}
