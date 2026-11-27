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

export type BeforeLeave = (go: () => void, whole?: boolean) => void

const askers: BeforeLeave[] = []
const asker = (): BeforeLeave | undefined => askers.at(-1)
let index = 0
let generation = 0
let flight: number | null = null
let after: (() => void) | null = null
let trip = 0
const GIVE_UP_MS = 1000
let unnumbered = false
let accepted = typeof window === 'undefined' ? '/' : window.location.pathname
let watching = false

const undoing = () => flight !== null && flight === index

function snapshot(): string {
  if (!undoing()) accepted = window.location.pathname
  return accepted
}

function numbered(state: unknown): number | undefined {
  const idx = (state as { idx?: unknown } | null)?.idx
  return typeof idx === 'number' ? idx : undefined
}

function moved() {
  generation += 1
  notify()
}

function landed() {
  const next = after
  after = null
  next?.()
}

function travel(from: number, to: number) {
  flight = to
  const mine = ++trip
  window.history.go(to - from)
  setTimeout(() => {
    if (trip !== mine || flight !== to) return
    flight = null
    const here = numbered(window.history.state)
    if (here === undefined) unnumbered = true
    else index = here
    moved()
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
      unnumbered = true
      flight = null
      after = null
      moved()
      return
    }
    if (unnumbered) {
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
        index = to
        moved()
        return
      }
      landed()
      return
    }
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

export function useBeforeLeave(ask: BeforeLeave | null) {
  const latest = useRef(ask)
  useLayoutEffect(() => {
    latest.current = ask
  })
  const on = ask !== null
  useEffect(() => {
    if (!on) return
    watchHistory()
    const mine: BeforeLeave = (go, whole) => (latest.current ? latest.current(go, whole) : go())
    askers.push(mine)
    return () => {
      const i = askers.lastIndexOf(mine)
      if (i >= 0) askers.splice(i, 1)
    }
  }, [on])
}

export function beforeLeaving(go: () => void) {
  const ask = asker()
  if (!ask) return go()
  generation += 1
  const mine = generation
  ask(() => {
    if (mine === generation) go()
  }, true)
}

export function navigate(path: string, { replace = false } = {}) {
  if (path === window.location.pathname) return
  watchHistory()
  if (replace) {
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
  const go = () => {
    if (flight !== null) after = push
    else push()
  }
  const ask = asker()
  if (!ask) return go()
  generation += 1
  const mine = generation
  ask(() => {
    if (mine === generation) go()
  })
}

export function usePath(): string {
  return useSyncExternalStore(subscribe, snapshot)
}

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
      return null
    }
  }
  return { workspace: Number(m[1]), section, ...(item ? { item } : {}) }
}

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

export function Link({ href, onClick, ...rest }: AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) {
  const click = (e: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(e)
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
    e.preventDefault()
    navigate(href)
  }
  return <a href={href} onClick={click} {...rest} />
}
