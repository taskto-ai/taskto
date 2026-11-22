// Test doubles for the server: a fetch that answers by method and path, and EventSources the
// test drives by hand.

import { vi } from 'vitest'
import type { EventSourceFactory } from '../conversations/useConversationStream'

// body is the JSON sent, parsed; any other body (a file's Blob, text that isn't JSON) as it was.
export type Call = { method: string; path: string; body: unknown }
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
    }
    calls.push(call)
    const route = routes[`${method} ${call.path}`] ?? routes[`${method} ${url.pathname}`]
    const reply =
      typeof route === 'function' ? await route(call) : (route ?? { status: 404, body: { error: 'not found' } })
    const status = reply.status ?? 200
    return new Response(status === 204 ? null : JSON.stringify(reply.body ?? {}), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
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
