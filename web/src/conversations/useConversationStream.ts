// Keeps a conversation's view current from its events stream (docs/api.md, "events"): the
// first event is the view, then each carries Chord operations to apply to it with
// applyImmutable. After the stream ends or drops, EventSource connects again by itself and
// the new stream starts from the current view; after a 404 or 503 it gives up, so this
// retries those itself, waiting longer each time, and stops for good once the conversation
// is gone (404 or 410 for the conversation itself), or the session is (401).

import { applyImmutable, type Op } from '@earendil-works/chord/delta'
import { useEffect, useRef, useState } from 'react'
import { ApiError, api } from '../api'
import type { ConversationView } from './transcript'

export type StreamStatus = 'connecting' | 'live' | 'reconnecting' | 'gone'

type EventSourceLike = Pick<EventSource, 'close' | 'readyState'> & {
  onopen: ((ev: Event) => unknown) | null
  onmessage: ((ev: MessageEvent) => unknown) | null
  onerror: ((ev: Event) => unknown) | null
}
export type EventSourceFactory = (url: string) => EventSourceLike

const defaultFactory: EventSourceFactory = (url) => new EventSource(url)

// Reports whether the conversation at url is gone, or the caller can no longer see it.
export type GoneProbe = (url: string) => Promise<boolean>

// Through api(), so a 401 (the session expired while the page sat open) sends the user back
// to log in, and the stream stops.
const defaultProbe: GoneProbe = async (url) => {
  try {
    await api(url)
    return false
  } catch (e) {
    if (e instanceof ApiError) return e.status === 401 || e.status === 404 || e.status === 410
    return false // offline: try the stream again later
  }
}

// How long to wait before connecting again after the server refused the stream: RETRY_MS,
// doubling with each refusal in a row, up to RETRY_MAX_MS.
export const RETRY_MS = 3000
export const RETRY_MAX_MS = 60_000
const CLOSED = 2

export function useConversationStream(
  url: string,
  factory: EventSourceFactory = defaultFactory,
  probe: GoneProbe = defaultProbe,
): { view: ConversationView | null; status: StreamStatus } {
  const [view, setView] = useState<ConversationView | null>(null)
  const [status, setStatus] = useState<StreamStatus>('connecting')
  const factoryRef = useRef(factory)
  factoryRef.current = factory
  const probeRef = useRef(probe)
  probeRef.current = probe

  useEffect(() => {
    let current: ConversationView | null = null
    let source: EventSourceLike | null = null
    let retry: ReturnType<typeof setTimeout> | undefined
    let stopped = false
    let refusals = 0
    // Restarts in a row for ops that didn't fit the view: a fresh view doesn't reset it, only
    // ops that apply, so a server that keeps sending ops that don't fit is backed off too.
    let misfits = 0
    setView(null)
    setStatus('connecting')

    const connect = () => {
      const es = factoryRef.current(url)
      source = es
      es.onopen = () => setStatus('live')
      es.onmessage = (ev) => {
        let event: { type?: string; value?: ConversationView; ops?: Op[] }
        try {
          event = JSON.parse(ev.data)
        } catch {
          return
        }
        if (event.type === 'value' && event.value) {
          refusals = 0 // a working stream
          current = event.value
          setView(current)
        } else if (event.type === 'ops' && Array.isArray(event.ops) && current) {
          try {
            current = applyImmutable(current, event.ops)
            misfits = 0
            setView(current)
          } catch {
            // Ops that don't fit the view: start again from a fresh one, later each time.
            current = null
            es.close()
            schedule(Math.min(RETRY_MS * 2 ** misfits, RETRY_MAX_MS))
            misfits++
          }
        }
        // `end`: the server closes the stream next, and EventSource connects again.
      }
      let handled = false
      es.onerror = () => {
        setStatus('reconnecting')
        // Once per connection: another error from the same closed source doesn't probe again
        // or count as another refusal.
        if (es.readyState === CLOSED && !handled && es === source) {
          handled = true
          refused()
        }
      }
    }
    const schedule = (delay = RETRY_MS) => {
      if (stopped) return
      setStatus('reconnecting')
      clearTimeout(retry)
      retry = setTimeout(connect, delay)
    }
    // The server refused the stream: if the conversation is gone, stop; else try again,
    // later each time.
    const refused = () => {
      const delay = Math.min(RETRY_MS * 2 ** refusals, RETRY_MAX_MS)
      refusals++
      probeRef.current(url.replace(/\/events$/, '')).then((gone) => {
        if (stopped) return
        if (gone) {
          stopped = true
          setStatus('gone')
          return
        }
        schedule(delay)
      })
    }

    connect()
    return () => {
      stopped = true
      clearTimeout(retry)
      source?.close()
    }
  }, [url])

  return { view, status }
}
