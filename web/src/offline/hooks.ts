// What the pages read of the offline machinery: the user's runtime, and the downloads and the queue
// in it, kept current as they change.

import { useCallback, useEffect, useState, useSyncExternalStore } from 'react'
import { type Runtime, type RuntimeState, runtimeState, subscribeRuntime } from './runtime'
import type { Download, OutboxItem, Scope } from './store'
import type { SyncStatus } from './sync'

export function useRuntime(): RuntimeState {
  return useSyncExternalStore(subscribeRuntime, runtimeState)
}

// Whether this tab is the one that changes existing records (writer.ts): the others read.
export function useWriter(runtime: Runtime | null): boolean {
  const subscribe = useCallback((fn: () => void) => runtime?.writer.subscribe(fn) ?? (() => {}), [runtime])
  return useSyncExternalStore(subscribe, () => runtime?.writer.holds() ?? false)
}

export type OfflineData = {
  loaded: boolean
  downloads: Download[]
  items: OutboxItem[]
  status: SyncStatus
}

const IDLE: SyncStatus = { paused: null, running: false, online: true }

// Runs `read` for each call to the function it returns, one at a time and in order: a call made
// while one runs is remembered, and one more read follows (however many were asked for), so a
// burst of announcements costs at most two reads and an older read never finishes after a newer
// one began. After `stop` nothing more is started.
export function serialReads(read: () => Promise<void>): { run: () => void; stop: () => void } {
  let running = false
  let again = false
  let stopped = false
  const loop = async () => {
    running = true
    try {
      do {
        again = false
        await read()
      } while (again && !stopped)
    } finally {
      running = false
    }
  }
  return {
    run: () => {
      if (stopped) return
      if (running) again = true
      else void loop()
    },
    stop: () => {
      stopped = true
    },
  }
}

// The downloads and the queue of a runtime (of one app, with a scope), read again whenever the
// store or the engine says something changed, or this tab's role as the writer did: the other tabs
// announce their changes (`OfflineStore.connectTabs`), a refusal says the view is stale
// (`OfflineStore.refresh`). Reads are one at a time and the last one asked for is the last shown.
export function useOfflineData(runtime: Runtime | null, scope?: Scope): OfflineData {
  const [data, setData] = useState<OfflineData>({ loaded: false, downloads: [], items: [], status: IDLE })
  const workspace = scope?.workspace
  const space = scope?.space
  const app = scope?.app
  useEffect(() => {
    if (!runtime) {
      setData({ loaded: false, downloads: [], items: [], status: IDLE })
      return
    }
    let current = true
    const reads = serialReads(async () => {
      try {
        const of =
          workspace !== undefined && space !== undefined && app !== undefined ? { workspace, space, app } : undefined
        const [downloads, items] = await Promise.all([runtime.store.listDownloads(), runtime.store.listItems(of)])
        // Nothing is shown from a runtime or an app that is not the one on screen any more.
        if (current) setData({ loaded: true, downloads, items, status: runtime.engine.status() })
      } catch {
        // The database was closed (signing out): nothing more to show.
      }
    })
    reads.run()
    const a = runtime.store.subscribe(reads.run)
    const b = runtime.engine.subscribe(reads.run)
    // This tab becoming the writer (or ceasing to be) means the queue may have been changed by the
    // tab that held the right meanwhile: what is shown is read again, never kept from before.
    const c = runtime.writer.subscribe(reads.run)
    return () => {
      current = false
      reads.stop()
      a()
      b()
      c()
    }
  }, [runtime, workspace, space, app])
  return data
}
