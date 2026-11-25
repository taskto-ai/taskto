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

// The downloads and the queue of a runtime (of one app, with a scope), read again whenever the
// store or the engine says something changed.
export function useOfflineData(runtime: Runtime | null, scope?: Scope): OfflineData {
  const [data, setData] = useState<OfflineData>({ loaded: false, downloads: [], items: [], status: IDLE })
  const workspace = scope?.workspace
  const space = scope?.space
  const app = scope?.app
  const read = useCallback(async () => {
    if (!runtime) return
    try {
      const of =
        workspace !== undefined && space !== undefined && app !== undefined ? { workspace, space, app } : undefined
      const [downloads, items] = await Promise.all([runtime.store.listDownloads(), runtime.store.listItems(of)])
      setData({ loaded: true, downloads, items, status: runtime.engine.status() })
    } catch {
      // The database was closed (signing out): nothing more to show.
    }
  }, [runtime, workspace, space, app])
  useEffect(() => {
    if (!runtime) {
      setData({ loaded: false, downloads: [], items: [], status: IDLE })
      return
    }
    void read()
    const a = runtime.store.subscribe(() => void read())
    const b = runtime.engine.subscribe(() => void read())
    // This tab becoming the writer (or ceasing to be) means the queue may have been changed by the
    // tab that held the right meanwhile: what is shown is read again, never kept from before.
    const c = runtime.writer.subscribe(() => void read())
    return () => {
      a()
      b()
      c()
    }
  }, [runtime, read])
  return data
}
