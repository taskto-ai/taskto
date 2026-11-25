// A small router on the History API: the server answers every path outside /api with the
// app (internal/webui), so a URL can be bookmarked and reloaded.

import { type AnchorHTMLAttributes, type MouseEvent, useEffect, useRef, useSyncExternalStore } from 'react'

const listeners = new Set<() => void>()

function notify() {
  for (const listener of listeners) listener()
}

function subscribe(listener: () => void) {
  watchHistory()
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

// --- Asking before leaving -----------------------------------------------------------------
//
// A page that would lose something if the person left it (a draft) says so with useBeforeLeave:
// then every move to another page asks it first. A new page (navigate, a Link, the workspace
// switcher) waits for its answer; the browser's Back and Forward, which have already moved when
// the app hears of them, are undone at once and done again when it says go. A replace (the shell
// putting an address in its usual form) is not a move and is never asked about. Closing the tab
// is the page's own (beforeunload).
//
// To undo and redo Back and Forward, the app numbers the history entries it makes (`idx` in their
// state). An entry it didn't number (made before it started) can't be undone: that one move goes
// on without the question.

// The page's question: called with what leaving does; it calls `go` when the person may leave (at
// once when there is nothing to lose), or never.
export type BeforeLeave = (go: () => void) => void

// The pages that ask, the latest last: it is the one asked.
const askers: BeforeLeave[] = []
const asker = (): BeforeLeave | undefined => askers.at(-1)
// Where the app is in the history it numbered.
let index = 0
// The popstate the app causes itself: the undoing of a move (`undo`), and the redoing of one the
// page agreed to (`redo`).
let undo = false
let redo = false
let watching = false

function numbered(state: unknown): number | undefined {
  const idx = (state as { idx?: unknown } | null)?.idx
  return typeof idx === 'number' ? idx : undefined
}

function watchHistory() {
  if (watching || typeof window === 'undefined') return
  watching = true
  const here = numbered(window.history.state)
  if (here === undefined) window.history.replaceState({ ...(window.history.state ?? {}), idx: index }, '')
  else index = here
  window.addEventListener('popstate', (e) => {
    const to = numbered(e.state)
    if (undo) {
      // Back where the person was: nothing changed for the page.
      undo = false
      return
    }
    const ask = asker()
    if (redo || !ask || to === undefined || to === index) {
      redo = false
      if (to !== undefined) index = to
      notify()
      return
    }
    const delta = to - index
    let now = true
    let agreed = false
    ask(() => {
      if (now) {
        agreed = true
        return
      }
      redo = true
      window.history.go(delta)
    })
    now = false
    if (agreed) {
      index = to
      notify()
      return
    }
    undo = true
    window.history.go(-delta)
  })
}

// While `ask` is given, moves to another page wait for it (see above). The latest page to ask is
// the one asked.
export function useBeforeLeave(ask: BeforeLeave | null) {
  const latest = useRef(ask)
  latest.current = ask
  const on = ask !== null
  useEffect(() => {
    if (!on) return
    watchHistory()
    const mine: BeforeLeave = (go) => (latest.current ? latest.current(go) : go())
    askers.push(mine)
    return () => {
      const i = askers.lastIndexOf(mine)
      if (i >= 0) askers.splice(i, 1)
    }
  }, [on])
}

export function navigate(path: string, { replace = false } = {}) {
  if (path === window.location.pathname) return
  watchHistory()
  if (replace) {
    window.history.replaceState({ idx: index }, '', path)
    notify()
    return
  }
  const go = () => {
    index += 1
    window.history.pushState({ idx: index }, '', path)
    notify()
  }
  const ask = asker()
  if (ask) ask(go)
  else go()
}

export function usePath(): string {
  return useSyncExternalStore(subscribe, () => window.location.pathname)
}

// 'notifications' is the inbox, the sidebar's entry with the unread count.
export type Section = 'home' | 'conversations' | 'knowledge' | 'boards' | 'apps' | 'settings' | 'notifications'
export const SECTIONS: readonly Section[] = [
  'home',
  'conversations',
  'knowledge',
  'boards',
  'apps',
  'settings',
  'notifications',
]

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
