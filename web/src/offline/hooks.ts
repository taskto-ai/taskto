import { useCallback, useEffect, useState, useSyncExternalStore } from 'react'
import { type Runtime, type RuntimeState, runtimeState, subscribeRuntime } from './runtime'
import type { Download, OutboxItem, Scope } from './store'
import type { SyncStatus } from './sync'

export function useRuntime(): RuntimeState {
  return useSyncExternalStore(subscribeRuntime, runtimeState)
}

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
        if (current) setData({ loaded: true, downloads, items, status: runtime.engine.status() })
      } catch {}
    })
    reads.run()
    const a = runtime.store.subscribe(reads.run)
    const b = runtime.engine.subscribe(reads.run)
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
