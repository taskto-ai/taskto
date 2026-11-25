// A small router on the History API: the server answers every path outside /api with the
// app (internal/webui), so a URL can be bookmarked and reloaded.

import {
  type AnchorHTMLAttributes,
  type MouseEvent,
  useEffect,
  useLayoutEffect,
  useRef,
  useSyncExternalStore,
} from 'react'

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
// The entry the app is on: where it last went, or agreed to go.
let index = 0
// One more for each question asked and each move made: an answer to an older question is stale,
// and does nothing (the person is asked again by whatever they do next).
let generation = 0
// The one move through the history of the app's own that is on its way, and where to: back to
// `index` (undoing the person's Back or Forward), or on to the entry the page agreed to. A popstate
// that lands on it is that move; one that lands elsewhere is the person's, made meanwhile.
let flight: number | null = null
// What waits for the undoing on its way to land first: an agreed Back or Forward, or an agreed new
// page (a push then would number its entry from where the address isn't yet).
let after: (() => void) | null = null
// Each move of the app's own: one that never lands (out of the history's range, so no popstate)
// is given up after a while, and what waited for it goes on.
let trip = 0
const GIVE_UP_MS = 1000
// Whether the address is on an entry the app didn't number (Back past where it started): the next
// numbered entry it comes to is where the app is, whatever its number.
let unnumbered = false
let watching = false

function numbered(state: unknown): number | undefined {
  const idx = (state as { idx?: unknown } | null)?.idx
  return typeof idx === 'number' ? idx : undefined
}

// A move made: the pages hear of it, and every question before it is stale.
function moved() {
  generation += 1
  notify()
}

// What waited for the app's own move to land.
function landed() {
  const next = after
  after = null
  next?.()
}

// The app's own move to the entry `to`, from the one the address is at.
function travel(from: number, to: number) {
  flight = to
  const mine = ++trip
  window.history.go(to - from)
  setTimeout(() => {
    if (trip !== mine || flight !== to) return
    flight = null
    landed()
  }, GIVE_UP_MS)
}

function watchHistory() {
  if (watching || typeof window === 'undefined') return
  watching = true
  const here = numbered(window.history.state)
  if (here === undefined) window.history.replaceState({ ...(window.history.state ?? {}), idx: index }, '')
  else index = here
  window.addEventListener('popstate', (e) => {
    const to = numbered(e.state)
    if (to === undefined) {
      // An entry from before the app numbered them: it can't be undone, so it goes on.
      unnumbered = true
      flight = null
      after = null
      moved()
      return
    }
    if (unnumbered) {
      // Back on an entry the app numbered, from one it didn't: that is where the app is now.
      unnumbered = false
      flight = null
      after = null
      index = to
      moved()
      return
    }
    if (flight !== null && to === flight) {
      flight = null
      if (to !== index) {
        // The move the page agreed to.
        index = to
        moved()
        return
      }
      // Back where the person was. An agreed move that waited for this goes now.
      landed()
      return
    }
    // The person's own move (one made while the app's was on its way included).
    flight = null
    after = null
    if (to === index) return
    const ask = asker()
    if (!ask) {
      index = to
      moved()
      return
    }
    generation += 1
    const mine = generation
    let now = true
    let agreed = false
    ask(() => {
      if (now) {
        agreed = true
        return
      }
      if (mine !== generation) return
      // The undoing may still be on its way: then this goes when it has landed.
      if (flight === index) after = () => travel(index, to)
      else travel(index, to)
    })
    now = false
    if (agreed) {
      index = to
      moved()
      return
    }
    travel(to, index)
  })
}

// While `ask` is given, moves to another page wait for it (see above). The latest page to ask is
// the one asked; it is told what the page last committed, never a render React set aside.
export function useBeforeLeave(ask: BeforeLeave | null) {
  const latest = useRef(ask)
  useLayoutEffect(() => {
    latest.current = ask
  })
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
    // An entry the app didn't number keeps it so: giving it `index` would make two of one number.
    window.history.replaceState(unnumbered ? window.history.state : { idx: index }, '', path)
    notify()
    return
  }
  const push = () => {
    index += 1
    unnumbered = false
    window.history.pushState({ idx: index }, '', path)
    moved()
  }
  // While an undoing is on its way, the push waits for it to land, so it numbers its entry from
  // where the address is.
  const go = () => {
    if (flight !== null) after = push
    else push()
  }
  const ask = asker()
  if (!ask) return go()
  generation += 1
  const mine = generation
  // An answer after another question or move is stale, and pushes nothing.
  ask(() => {
    if (mine === generation) go()
  })
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
