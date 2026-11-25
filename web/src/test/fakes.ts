// Test doubles for the server: a fetch that answers by method and path, and EventSources the
// test drives by hand.

import { vi } from 'vitest'
import type { EventSourceFactory } from '../conversations/useConversationStream'

// body is the JSON sent, parsed; any other body (a file's Blob, text that isn't JSON) as it was.
export type Call = {
  method: string
  path: string
  body: unknown
  headers: Record<string, string>
  signal?: AbortSignal
  // Settles when the app has read the answer's body: proof that a late answer was delivered.
  read?: Promise<void>
}
type Answer = { status?: number; body?: unknown }
// A function may answer later, to hold a request open.
type Reply = Answer | ((call: Call) => Answer | Promise<Answer>)

// routes: "METHOD /path" → reply. An unknown route is a 404, and every call is recorded.
function parseBody(body: BodyInit | null | undefined): unknown {
  if (typeof body !== 'string') return body ?? undefined
  try {
    return JSON.parse(body)
  } catch {
    return body
  }
}

export function fakeFetch(routes: Record<string, Reply>) {
  const calls: Call[] = []
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const method = init?.method ?? 'GET'
    const call = {
      method,
      path: url.pathname + url.search,
      body: parseBody(init?.body),
    } as Call
    // The headers are there to be read, but not part of what a test compares calls by.
    Object.defineProperty(call, 'headers', {
      value: Object.fromEntries(new Headers(init?.headers).entries()),
      enumerable: false,
    })
    // So is the signal a request can be aborted with: a held-open route watches it.
    Object.defineProperty(call, 'signal', { value: init?.signal ?? undefined, enumerable: false })
    let markRead!: () => void
    Object.defineProperty(call, 'read', {
      value: new Promise<void>((resolve) => {
        markRead = resolve
      }),
      enumerable: false,
    })
    calls.push(call)
    // As a real fetch does: a request whose signal aborts, before or while it is answered, ends
    // with an AbortError, whatever the route would have said.
    const signal = init?.signal ?? undefined
    const aborted = () => signal?.reason ?? new DOMException('This operation was aborted', 'AbortError')
    if (signal?.aborted) throw aborted()
    const route = routes[`${method} ${call.path}`] ?? routes[`${method} ${url.pathname}`]
    const answer = async () =>
      typeof route === 'function' ? await route(call) : (route ?? { status: 404, body: { error: 'not found' } })
    const reply = signal
      ? await Promise.race([
          answer(),
          new Promise<never>((_, reject) => signal.addEventListener('abort', () => reject(aborted()), { once: true })),
        ])
      : await answer()
    const status = reply.status ?? 200
    const res = new Response(status === 204 ? null : JSON.stringify(reply.body ?? {}), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
    const json = res.json.bind(res)
    res.json = async () => {
      try {
        return await json()
      } finally {
        markRead()
      }
    }
    if (status === 204) markRead()
    return res
  })
  vi.stubGlobal('fetch', fn)
  return calls
}

export class FakeEventSource {
  readyState = 0
  onopen: ((ev: Event) => unknown) | null = null
  onmessage: ((ev: MessageEvent) => unknown) | null = null
  onerror: ((ev: Event) => unknown) | null = null
  closed = false
  constructor(readonly url: string) {}
  close() {
    this.closed = true
    this.readyState = 2
  }
  open() {
    this.readyState = 1
    this.onopen?.(new Event('open'))
  }
  send(data: unknown) {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(data) }))
  }
  fail(closed: boolean) {
    this.readyState = closed ? 2 : 0
    this.onerror?.(new Event('error'))
  }
}

export function fakeStreams() {
  const sources: FakeEventSource[] = []
  const factory: EventSourceFactory = (url) => {
    const es = new FakeEventSource(url)
    sources.push(es)
    return es
  }
  return { sources, factory }
}
