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

export type GoneProbe = (url: string) => Promise<boolean>

const defaultProbe: GoneProbe = async (url) => {
  try {
    await api(url)
    return false
  } catch (e) {
    if (e instanceof ApiError) return e.status === 401 || e.status === 404 || e.status === 410
    return false
  }
}

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
          refusals = 0
          current = event.value
          setView(current)
        } else if (event.type === 'ops' && Array.isArray(event.ops) && current) {
          try {
            current = applyImmutable(current, event.ops)
            misfits = 0
            setView(current)
          } catch {
            current = null
            es.close()
            schedule(Math.min(RETRY_MS * 2 ** misfits, RETRY_MAX_MS))
            misfits++
          }
        }
      }
      let handled = false
      es.onerror = () => {
        setStatus('reconnecting')
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
